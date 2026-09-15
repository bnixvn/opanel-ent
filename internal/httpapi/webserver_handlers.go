package httpapi

import (
	"fmt"
	"net/http"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
)

// handleWebserverBackends reports which webservers this host can run, which
// one is running, and what LiteSpeed Enterprise would cost to turn on.
func (s *Server) handleWebserverBackends(w http.ResponseWriter, r *http.Request) {
	list, err := agentclient.Call[actions.WebserverListResult](r.Context(), s.agent,
		"webserver.backends", 1, struct{}{})
	if err != nil {
		s.agentError(w, err)
		return
	}
	// LiteSpeed's own state is a separate question from the backend list: it
	// is the only backend that has a licence, and a licence that has expired
	// is the difference between "installed" and "usable".
	lsws, err := agentclient.Call[actions.LSWSStatusResult](r.Context(), s.agent,
		"lsws.status", 1, struct{}{})
	if err != nil {
		s.agentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"active":       list.Active,
		"backends":     list.Backends,
		"php_provider": list.PHP,
		"php_versions": list.Versions,
		"litespeed":    lsws,
	})
}

type webserverSwitchBody struct {
	Backend string `json:"backend"`
}

// handleWebserverSwitch moves the host to a different webserver.
func (s *Server) handleWebserverSwitch(w http.ResponseWriter, r *http.Request) {
	var body webserverSwitchBody
	if !decodeJSON(w, r, &body) {
		return
	}
	// A switch renders every site, writes every pool, and hands the ports
	// from one daemon to another. On a host with a few hundred sites that is
	// minutes, and the agent's own budget for it is fifteen.
	allowLongResponse(w, actions.SwitchBudget+time.Minute)
	res, err := s.sites.SwitchWebserver(r.Context(), body.Backend)
	if err != nil {
		s.agentError(w, err)
		return
	}
	s.audit(r, "webserver.switch", res.Backend, true, "switched from "+res.Previous)
	writeJSON(w, http.StatusOK, res)
}

type litespeedInstallBody struct {
	// Serial is the licence the operator bought. Empty asks for the vendor's
	// fifteen-day trial key.
	Serial string `json:"serial"`
}

// handleLiteSpeedInstall installs LiteSpeed Enterprise, leaving it stopped.
func (s *Server) handleLiteSpeedInstall(w http.ResponseWriter, r *http.Request) {
	var body litespeedInstallBody
	if !decodeJSON(w, r, &body) {
		return
	}
	res, err := agentclient.Call[actions.LSWSStatusResult](r.Context(), s.agent,
		"lsws.install", 1, actions.LSWSInstallRequest{Serial: body.Serial})
	if err != nil {
		s.agentError(w, err)
		return
	}
	licence := res.Licence
	if licence == "" {
		licence = "none"
	}
	s.audit(r, "litespeed.install", res.Version, true, "licence "+licence)
	writeJSON(w, http.StatusOK, res)
}

// phpProviderBody names the provider to move to.
type phpProviderBody struct {
	Provider string `json:"provider"`
}

// handlePHPProvider reports where this host's PHP comes from.
func (s *Server) handlePHPProvider(w http.ResponseWriter, r *http.Request) {
	st, err := agentclient.Call[actions.PHPProviderStatus](r.Context(), s.agent,
		"php.provider", 1, struct{}{})
	if err != nil {
		s.agentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handlePHPProviderSet moves every site onto a different source of PHP.
//
// The reason an operator does this is LiteSpeed: it can be handed one of
// CloudLinux's interpreters and cannot be handed one of Remi's, so a host
// that wants both servers to run the same build has to be on alt-php. It is
// a migration, not a setting -- every pool is rewritten and every vhost
// re-rendered -- which is why it takes minutes and has its own endpoint.
func (s *Server) handlePHPProviderSet(w http.ResponseWriter, r *http.Request) {
	var body phpProviderBody
	if !decodeJSON(w, r, &body) {
		return
	}
	allowLongResponse(w, actions.PHPProviderBudget+time.Minute)
	res, err := s.sites.SetPHPProvider(r.Context(), body.Provider)
	if err != nil {
		s.audit(r, "php.provider", body.Provider, false, err.Error())
		s.agentError(w, err)
		return
	}
	s.audit(r, "php.provider", res.Provider, true,
		fmt.Sprintf("moved %d site(s) from %s", res.Migrated, res.Previous))
	writeJSON(w, http.StatusOK, res)
}
