package filemanager

import (
	"context"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
)

// Copy duplicates a file or a whole directory within one home.
func (s *Service) Copy(ctx context.Context, owner Owner, from, to string) error {
	_, err := agentclient.Call[struct{}](ctx, s.agent, "fs.copy", 1,
		actions.FileCopyRequest{Owner: owner.Username, From: clean(from), To: clean(to)})
	return err
}

// ChmodRecursive applies a mode to a whole tree.
func (s *Service) ChmodRecursive(ctx context.Context, owner Owner, p, mode string) error {
	_, err := agentclient.Call[struct{}](ctx, s.agent, "fs.chmod_recursive", 1,
		actions.FileChmodRequest{Owner: owner.Username, Path: clean(p), Mode: mode})
	return err
}

// Archive packs the given paths into one archive file.
func (s *Service) Archive(ctx context.Context, owner Owner, paths []string, dest string) error {
	cleaned := make([]string, 0, len(paths))
	for _, p := range paths {
		cleaned = append(cleaned, clean(p))
	}
	_, err := agentclient.Call[struct{}](ctx, s.agent, "fs.archive", 1,
		actions.FileArchiveRequest{Owner: owner.Username, Paths: cleaned, Dest: clean(dest)})
	return err
}

// Extract unpacks an archive into a directory.
func (s *Service) Extract(ctx context.Context, owner Owner, p, dest string) (actions.FileExtractResult, error) {
	return agentclient.Call[actions.FileExtractResult](ctx, s.agent, "fs.extract", 1,
		actions.FileExtractRequest{Owner: owner.Username, Path: clean(p), Dest: clean(dest)})
}
