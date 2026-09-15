package actions

import (
	"testing"

	"github.com/bnixvn/opanel-ent/internal/webserver"
)

// Both servers read the same rules, because LiteSpeed Enterprise reads
// Apache's configuration -- that is the whole reason they share a renderer.
// Rules written into a LiteSpeed-specific directory would be rules nothing
// includes, which is what used to happen: switching a host with 658 rules
// loaded reported it as having no firewall at all.
func TestWAFRulesAreTheSameFileOnBothServers(t *testing.T) {
	apachePaths := wafPathsFor(webserver.BackendApache)
	lswsPaths := wafPathsFor(webserver.BackendLSWS)

	if apachePaths.RulesFile != lswsPaths.RulesFile {
		t.Errorf("rules live in two places: %q under Apache, %q under LiteSpeed",
			apachePaths.RulesFile, lswsPaths.RulesFile)
	}
	if apachePaths.Dir != lswsPaths.Dir {
		t.Errorf("rule directories differ: %q and %q", apachePaths.Dir, lswsPaths.Dir)
	}
	// LiteSpeed Enterprise carries its own engine, so there is no package to
	// install. The old value named OpenLiteSpeed's, from a repository this
	// panel no longer uses, for a server it no longer supports.
	if lswsPaths.Package != "" {
		t.Errorf("LiteSpeed offered to install %q; its engine is built in", lswsPaths.Package)
	}
	if apachePaths.Package == "" {
		t.Error("Apache needs a package and none is named")
	}
	// Each server has to look for its engine in its own place, or the panel
	// reports the other server's state.
	if apachePaths.Module == lswsPaths.Module {
		t.Errorf("both servers probe %q for the engine", apachePaths.Module)
	}
}

// LiteSpeed cannot open the audit log the rules name -- /var/log/httpd is
// 0700 root and its workers run as nobody -- so every block it made was
// invisible on the events page while the firewall was demonstrably working.
// It writes them into its own error log instead, one line per event, and the
// parser has to read that shape as well as the multi-line audit records.
func TestWAFEventsFromAnErrorLogLine(t *testing.T) {
	const line = `[Tue Sep 15 21:33:03.918184 2026] [error] [client 127.0.0.1] ` +
		`ModSecurity: Access denied with code 403, ` +
		`[Rule: 'TX:BLOCKING_INBOUND_ANOMALY_SCORE' '@ge %{tx.inbound_anomaly_score_threshold}'] ` +
		`[id "949110"] [msg "Inbound Anomaly Score Exceeded (Total Score: 15)"] ` +
		`[tag "OWASP_CRS"] [hostname "demo1.opanel.test"] ` +
		`[uri "/probe.php?id=1%27%20UNION%20SELECT%20NULL"] [unique_id "dqz673IzOe"]`

	got := parseWAFEvents(line+"\n"+line, 10)
	if len(got) != 2 {
		t.Fatalf("parsed %d events from two lines, want 2", len(got))
	}
	e := got[0]
	if e.RuleID != "949110" {
		t.Errorf("rule id %q", e.RuleID)
	}
	if e.Host != "demo1.opanel.test" {
		t.Errorf("host %q", e.Host)
	}
	if !e.Blocked {
		t.Error("an Access denied line was not recorded as blocked")
	}
	if e.ClientIP != "127.0.0.1" {
		t.Errorf("client %q", e.ClientIP)
	}
	if e.URI == "" {
		t.Error("no uri")
	}
}
