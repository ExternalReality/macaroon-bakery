package idservice_test

import (
	"fmt"
	"log"
	"net"
	"net/http"

	"gopkg.in/errgo.v1"

	"gopkg.in/macaroon-bakery.v0/bakery"
	"gopkg.in/macaroon-bakery.v0/bakery/checkers"
	"gopkg.in/macaroon-bakery.v0/httpbakery"
)

type targetServiceHandler struct {
	svc          *bakery.Service
	authEndpoint string
	endpoint     string
	mux          *http.ServeMux
}

// targetService implements a "target service", representing
// an arbitrary web service that wants to delegate authorization
// to third parties.
func targetService(endpoint, authEndpoint string, authPK *bakery.PublicKey) (http.Handler, error) {
	key, err := bakery.GenerateKey()
	if err != nil {
		return nil, err
	}
	pkLocator := bakery.NewPublicKeyRing()
	svc, err := bakery.NewService(bakery.NewServiceParams{
		Key:      key,
		Location: endpoint,
		Locator:  pkLocator,
	})
	if err != nil {
		return nil, err
	}
	log.Printf("adding public key for location %s: %v", authEndpoint, authPK)
	pkLocator.AddPublicKeyForLocation(authEndpoint, true, authPK)
	mux := http.NewServeMux()
	srv := &targetServiceHandler{
		svc:          svc,
		authEndpoint: authEndpoint,
	}
	mux.HandleFunc("/gold/", srv.serveGold)
	mux.HandleFunc("/silver/", srv.serveSilver)
	return mux, nil
}

func (srv *targetServiceHandler) serveGold(w http.ResponseWriter, req *http.Request) {
	checker := srv.checkers(req, "gold")
	if err := httpbakery.CheckRequest(srv.svc, req, checker); err != nil {
		srv.writeError(w, "gold", err)
		return
	}
	fmt.Fprintf(w, "all is golden")
}

func (srv *targetServiceHandler) serveSilver(w http.ResponseWriter, req *http.Request) {
	checker := srv.checkers(req, "silver")
	if err := httpbakery.CheckRequest(srv.svc, req, checker); err != nil {
		srv.writeError(w, "silver", err)
		return
	}
	fmt.Fprintf(w, "every cloud has a silver lining")
}

// checkers implements the caveat checking for the service.
// Note how we add context-sensitive checkers
// (remote-host checks information from the HTTP request)
// to the standard checkers implemented by checkers.Std.
func (svc *targetServiceHandler) checkers(req *http.Request, operation string) bakery.FirstPartyChecker {
	m := checkers.Map{
		"remote-host": func(s string) error {
			// TODO(rog) do we want to distinguish between
			// the two kinds of errors below?
			_, host, err := checkers.ParseCaveat(s)
			if err != nil {
				return err
			}
			remoteHost, _, err := net.SplitHostPort(req.RemoteAddr)
			if err != nil {
				return fmt.Errorf("cannot parse request remote address")
			}
			if remoteHost != host {
				return fmt.Errorf("remote address mismatch (need %q, got %q)", host, remoteHost)
			}
			return nil
		},
		"operation": func(s string) error {
			_, op, err := checkers.ParseCaveat(s)
			if err != nil {
				return err
			}
			if op != operation {
				return fmt.Errorf("macaroon not valid for operation")
			}
			return nil
		},
	}
	return checkers.PushFirstPartyChecker(m, checkers.Std)
}

// writeError writes an error to w. If the error was generated because
// of a required macaroon that the client does not have, we mint a
// macaroon that, when discharged, will grant the client the
// right to execute the given operation.
//
// The logic in this function is crucial to the security of the service
// - it must determine for a given operation what caveats to attach.
func (srv *targetServiceHandler) writeError(w http.ResponseWriter, operation string, verr error) {
	fail := func(code int, msg string, args ...interface{}) {
		if code == http.StatusInternalServerError {
			msg = "internal error: " + msg
		}
		http.Error(w, fmt.Sprintf(msg, args...), code)
	}

	if _, ok := errgo.Cause(verr).(*bakery.VerificationError); !ok {
		fail(http.StatusForbidden, "%v", verr)
		return
	}

	// Work out what caveats we need to apply for the given operation.
	// Could special-case the operation here if desired.
	caveats := []bakery.Caveat{
		checkers.ThirdParty(srv.authEndpoint, "member-of-group target-service-users"),
		checkers.FirstParty("operation " + operation),
	}
	// Mint an appropriate macaroon and send it back to the client.
	m, err := srv.svc.NewMacaroon("", nil, caveats)
	if err != nil {
		fail(http.StatusInternalServerError, "cannot mint macaroon: %v", err)
		return
	}
	httpbakery.WriteDischargeRequiredError(w, m, verr)
}
