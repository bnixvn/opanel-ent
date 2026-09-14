package actions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	miniocreds "github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/bnixvn/opanel-ent/internal/agent"
)

// DestinationDir holds one file per configured destination.
//
// The secret lives here and nowhere else. The panel keeps the name, the host
// and the path -- everything needed to show a list -- and never holds the
// password or the access key, the same way it never holds a customer's
// database password. A copy of the panel database is not a copy of anybody's
// storage credentials.
const DestinationDir = "/var/lib/opanel/destinations"

// DestinationBudget bounds an upload. A large account's archive over a slow
// link is minutes, not seconds.
const DestinationBudget = 2 * time.Hour

// Destination kinds.
const (
	DestSFTP = "sftp"
	DestS3   = "s3"
)

// DestinationConfig is everything needed to reach a remote store.
type DestinationConfig struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"`

	// SFTP.
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	User string `json:"user,omitempty"`
	// Path is the directory on the far side, for SFTP, or the prefix inside
	// the bucket, for S3.
	Path string `json:"path,omitempty"`
	// HostKey is the far side's public key in authorized_keys form. Without
	// it the first connection has nothing to check against, and an upload
	// carrying every customer's files is not a thing to hand to whoever
	// answers the address today.
	HostKey string `json:"host_key,omitempty"`

	// S3.
	Bucket   string `json:"bucket,omitempty"`
	Region   string `json:"region,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	// AccessKey is not a secret on its own; the secret key is.
	AccessKey string `json:"access_key,omitempty"`

	// Secret is the password, the private key or the S3 secret key. It is
	// written to this file and never sent back.
	Secret string `json:"secret,omitempty"`
	// SecretKind is "password" or "key", for SFTP.
	SecretKind string `json:"secret_kind,omitempty"`
}

// Validate checks a destination before it is stored.
func (c *DestinationConfig) Validate() error {
	if c.ID <= 0 {
		return errors.New("a destination id is required")
	}
	switch c.Kind {
	case DestSFTP:
		if c.Host == "" {
			return errors.New("a host is required")
		}
		if strings.ContainsAny(c.Host, " \t/\\") {
			return fmt.Errorf("%q is not a hostname", c.Host)
		}
		if c.Port < 0 || c.Port > 65535 {
			return errors.New("the port is out of range")
		}
		if c.User == "" {
			return errors.New("a username is required")
		}
		if c.SecretKind != "password" && c.SecretKind != "key" {
			return errors.New("the credential must be a password or a private key")
		}
		if c.HostKey == "" {
			return errors.New("the server's host key is required, so the panel " +
				"can tell the right server from whoever answers the address")
		}
		if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(c.HostKey)); err != nil {
			return fmt.Errorf("that host key is not in authorized_keys form: %w", err)
		}
	case DestS3:
		if c.Bucket == "" {
			return errors.New("a bucket is required")
		}
		if c.AccessKey == "" {
			return errors.New("an access key is required")
		}
		if c.Endpoint != "" {
			if _, err := url.Parse("https://" + strings.TrimPrefix(
				strings.TrimPrefix(c.Endpoint, "https://"), "http://")); err != nil {
				return fmt.Errorf("%q is not an endpoint", c.Endpoint)
			}
		}
	default:
		return fmt.Errorf("%q is not a kind of destination this panel supports", c.Kind)
	}
	// A path with a traversal in it would put a customer's archive somewhere
	// nobody meant, on a server the panel does not own.
	if strings.Contains(c.Path, "..") {
		return fmt.Errorf("%q is not a usable path", c.Path)
	}
	return nil
}

// DestinationRef names a stored destination.
type DestinationRef struct {
	ID int64 `json:"id"`
}

// Validate checks the id.
func (r *DestinationRef) Validate() error {
	if r.ID <= 0 {
		return errors.New("a destination id is required")
	}
	return nil
}

// DestinationUploadRequest sends one archive.
type DestinationUploadRequest struct {
	ID int64 `json:"id"`
	// Path is the archive on this server.
	Path string `json:"path"`
	// Name is what it should be called on the far side.
	Name string `json:"name"`
}

// Validate checks the archive and the name it will take.
func (r *DestinationUploadRequest) Validate() error {
	if r.ID <= 0 {
		return errors.New("a destination id is required")
	}
	if !strings.HasPrefix(filepath.Clean(r.Path), BackupRoot+"/") {
		return fmt.Errorf("%q is not a backup this panel made", r.Path)
	}
	if r.Name == "" || strings.ContainsAny(r.Name, "/\\") || strings.Contains(r.Name, "..") {
		return fmt.Errorf("%q is not a usable file name", r.Name)
	}
	return nil
}

// DestinationResult reports what happened.
type DestinationResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
	// Remote is where the archive ended up, for the record.
	Remote string `json:"remote,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
}

func registerDestinations(r *agent.Registry) {
	agent.Register(r, "dest.save", 1, func(_ context.Context, in DestinationConfig) (DestinationResult, error) {
		return DestinationResult{OK: true}, saveDestination(&in)
	})

	agent.Register(r, "dest.delete", 1, func(_ context.Context, in DestinationRef) (DestinationResult, error) {
		err := os.Remove(destPath(in.ID))
		if err != nil && !os.IsNotExist(err) {
			return DestinationResult{}, err
		}
		return DestinationResult{OK: true}, nil
	})

	agent.RegisterSlow(r, "dest.test", 1, 3*time.Minute,
		func(ctx context.Context, in DestinationRef) (DestinationResult, error) {
			return testDestination(ctx, in.ID)
		})

	agent.RegisterSlow(r, "dest.upload", 1, DestinationBudget,
		func(ctx context.Context, in DestinationUploadRequest) (DestinationResult, error) {
			return uploadToDestination(ctx, in)
		})
}

func destPath(id int64) string {
	return filepath.Join(DestinationDir, strconv.FormatInt(id, 10)+".json")
}

// saveDestination writes the configuration, secret and all, root-only.
func saveDestination(c *DestinationConfig) error {
	if err := os.MkdirAll(DestinationDir, 0o700); err != nil {
		return err
	}
	// An empty secret on an update means "keep the one you have": the panel
	// cannot send back what it never received.
	if c.Secret == "" {
		if existing, err := loadDestination(c.ID); err == nil {
			c.Secret = existing.Secret
		}
	}
	if c.Secret == "" {
		return errors.New("a password, private key or secret key is required")
	}
	body, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(destPath(c.ID), body, 0o600)
}

func loadDestination(id int64) (*DestinationConfig, error) {
	body, err := os.ReadFile(destPath(id))
	if err != nil {
		return nil, fmt.Errorf("that destination is not configured on this server")
	}
	var c DestinationConfig
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// testDestination proves the credentials work, by writing and removing a
// probe rather than only connecting: permission to log in is not permission
// to write, and finding that out during the first real backup is too late.
func testDestination(ctx context.Context, id int64) (DestinationResult, error) {
	c, err := loadDestination(id)
	if err != nil {
		return DestinationResult{}, err
	}
	probe := fmt.Sprintf("opanel-probe-%d.txt", time.Now().Unix())
	body := []byte("OPanel connection test. Safe to delete.\n")

	switch c.Kind {
	case DestSFTP:
		client, closeFn, err := dialSFTP(ctx, c)
		if err != nil {
			return DestinationResult{}, err
		}
		defer closeFn()

		remote := path.Join(c.Path, probe)
		if c.Path != "" {
			if err := mkdirAllSFTP(client, c.Path); err != nil {
				return DestinationResult{}, fmt.Errorf("create %s: %w", c.Path, err)
			}
		}
		f, err := client.Create(remote)
		if err != nil {
			return DestinationResult{}, fmt.Errorf("write to %s: %w", remote, err)
		}
		if _, err := f.Write(body); err != nil {
			_ = f.Close()
			return DestinationResult{}, err
		}
		if err := f.Close(); err != nil {
			return DestinationResult{}, err
		}
		if err := client.Remove(remote); err != nil {
			return DestinationResult{OK: true,
				Message: "Uploads work, but the panel could not delete its test file. " +
					"Old backups will not be cleaned up: " + err.Error()}, nil
		}
		return DestinationResult{OK: true, Message: "Connected, wrote and removed a test file."}, nil

	default: // S3
		client, err := dialS3(c)
		if err != nil {
			return DestinationResult{}, err
		}
		key := path.Join(c.Path, probe)
		if _, err := client.PutObject(ctx, c.Bucket, key,
			strings.NewReader(string(body)), int64(len(body)),
			minio.PutObjectOptions{ContentType: "text/plain"}); err != nil {
			return DestinationResult{}, fmt.Errorf("write to %s: %w", c.Bucket, err)
		}
		if err := client.RemoveObject(ctx, c.Bucket, key, minio.RemoveObjectOptions{}); err != nil {
			return DestinationResult{OK: true,
				Message: "Uploads work, but the panel could not delete its test object. " +
					"Old backups will not be cleaned up: " + err.Error()}, nil
		}
		return DestinationResult{OK: true, Message: "Connected, wrote and removed a test object."}, nil
	}
}

// uploadToDestination sends one archive.
func uploadToDestination(ctx context.Context, in DestinationUploadRequest) (DestinationResult, error) {
	c, err := loadDestination(in.ID)
	if err != nil {
		return DestinationResult{}, err
	}
	src, err := os.Open(in.Path)
	if err != nil {
		return DestinationResult{}, err
	}
	defer func() { _ = src.Close() }()
	info, err := src.Stat()
	if err != nil {
		return DestinationResult{}, err
	}

	switch c.Kind {
	case DestSFTP:
		client, closeFn, err := dialSFTP(ctx, c)
		if err != nil {
			return DestinationResult{}, err
		}
		defer closeFn()

		if c.Path != "" {
			if err := mkdirAllSFTP(client, c.Path); err != nil {
				return DestinationResult{}, fmt.Errorf("create %s: %w", c.Path, err)
			}
		}
		// Written to a temporary name and renamed, so a transfer that dies
		// halfway does not leave something that looks like a usable backup.
		remote := path.Join(c.Path, in.Name)
		partial := remote + ".part"
		dst, err := client.Create(partial)
		if err != nil {
			return DestinationResult{}, fmt.Errorf("write to %s: %w", partial, err)
		}
		n, err := io.Copy(dst, src)
		if err != nil {
			_ = dst.Close()
			_ = client.Remove(partial)
			return DestinationResult{}, fmt.Errorf("upload: %w", err)
		}
		if err := dst.Close(); err != nil {
			_ = client.Remove(partial)
			return DestinationResult{}, err
		}
		_ = client.Remove(remote) // a previous copy of the same name
		if err := client.Rename(partial, remote); err != nil {
			return DestinationResult{}, fmt.Errorf("finish %s: %w", remote, err)
		}
		return DestinationResult{OK: true, Remote: remote, Bytes: n}, nil

	default: // S3
		client, err := dialS3(c)
		if err != nil {
			return DestinationResult{}, err
		}
		key := path.Join(c.Path, in.Name)
		// An object is not visible until it is complete, so there is no
		// partial-file problem to solve here.
		res, err := client.PutObject(ctx, c.Bucket, key, src, info.Size(),
			minio.PutObjectOptions{ContentType: "application/gzip"})
		if err != nil {
			return DestinationResult{}, fmt.Errorf("upload to %s: %w", c.Bucket, err)
		}
		return DestinationResult{OK: true, Remote: c.Bucket + "/" + key, Bytes: res.Size}, nil
	}
}

// dialSFTP opens a session, checking the host key.
func dialSFTP(ctx context.Context, c *DestinationConfig) (*sftp.Client, func(), error) {
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(c.HostKey))
	if err != nil {
		return nil, nil, fmt.Errorf("the stored host key is unusable: %w", err)
	}

	var auth []ssh.AuthMethod
	if c.SecretKind == "key" {
		signer, err := ssh.ParsePrivateKey([]byte(c.Secret))
		if err != nil {
			return nil, nil, fmt.Errorf("that private key could not be read " +
				"(an encrypted key needs its passphrase removed first)")
		}
		auth = append(auth, ssh.PublicKeys(signer))
	} else {
		auth = append(auth, ssh.Password(c.Secret))
	}

	port := c.Port
	if port == 0 {
		port = 22
	}
	cfg := &ssh.ClientConfig{
		User: c.User,
		Auth: auth,
		// Pinned, never InsecureIgnoreHostKey: this connection carries every
		// file a customer has, and trusting whatever answers the address is
		// how it ends up somewhere else.
		HostKeyCallback: ssh.FixedHostKey(pub),
		// Without this the client negotiates whichever host key type the
		// server prefers, which is usually not the one that was pinned, and
		// every connection fails with "host key mismatch" no matter how
		// correct the pin is.
		HostKeyAlgorithms: hostKeyAlgorithms(pub.Type()),
		Timeout:           30 * time.Second,
	}

	addr := net.JoinHostPort(c.Host, strconv.Itoa(port))
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("sign in to %s: %w", addr, err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)

	sc, err := sftp.NewClient(client)
	if err != nil {
		_ = client.Close()
		return nil, nil, fmt.Errorf("start sftp on %s: %w", addr, err)
	}
	return sc, func() { _ = sc.Close(); _ = client.Close() }, nil
}

// hostKeyAlgorithms lists what may be negotiated for a pinned key.
//
// One key type, with an exception for RSA: a server holding an ssh-rsa host
// key signs with rsa-sha2-256 or rsa-sha2-512 on anything modern, and
// offering only "ssh-rsa" would be refused by servers that have dropped
// SHA-1 -- which is most of them.
func hostKeyAlgorithms(keyType string) []string {
	if keyType == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	}
	return []string{keyType}
}

// mkdirAllSFTP creates a directory and its parents on the far side.
func mkdirAllSFTP(client *sftp.Client, dir string) error {
	if dir == "" || dir == "/" || dir == "." {
		return nil
	}
	if info, err := client.Stat(dir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", dir)
		}
		return nil
	}
	if err := mkdirAllSFTP(client, path.Dir(dir)); err != nil {
		return err
	}
	if err := client.Mkdir(dir); err != nil {
		// Another upload may have created it between the check and now.
		if info, statErr := client.Stat(dir); statErr == nil && info.IsDir() {
			return nil
		}
		return err
	}
	return nil
}

// dialS3 builds a client for S3 or anything that speaks its protocol.
func dialS3(c *DestinationConfig) (*minio.Client, error) {
	endpoint := c.Endpoint
	secure := true
	if endpoint == "" {
		endpoint = "s3.amazonaws.com"
	}
	if rest, ok := strings.CutPrefix(endpoint, "http://"); ok {
		// Plain HTTP is allowed for a MinIO on the same rack, and refused
		// for anything else: an archive of every customer's files should not
		// cross the internet in the clear.
		endpoint, secure = rest, false
		host, _, err := net.SplitHostPort(endpoint)
		if err != nil {
			host = endpoint
		}
		if ip := net.ParseIP(host); ip == nil || !ip.IsPrivate() && !ip.IsLoopback() {
			return nil, errors.New("plain http is only allowed to a private address; " +
				"use https for anything else")
		}
	}
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimSuffix(endpoint, "/")

	return minio.New(endpoint, &minio.Options{
		Creds:  miniocreds.NewStaticV4(c.AccessKey, c.Secret, ""),
		Secure: secure,
		Region: c.Region,
	})
}
