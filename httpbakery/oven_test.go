package httpbakery_test

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/juju/httprequest"
	jujutesting "github.com/juju/testing"
	jc "github.com/juju/testing/checkers"
	"golang.org/x/net/context"
	gc "gopkg.in/check.v1"
	"gopkg.in/errgo.v1"

	"gopkg.in/macaroon-bakery.v2-unstable/bakery"
	"gopkg.in/macaroon-bakery.v2-unstable/bakery/checkers"
	"gopkg.in/macaroon-bakery.v2-unstable/bakerytest"
	"gopkg.in/macaroon-bakery.v2-unstable/httpbakery"
)

type OvenSuite struct {
	jujutesting.LoggingSuite
}

var _ = gc.Suite(&OvenSuite{})

func (*OvenSuite) TestOvenWithAuthnMacaroon(c *gc.C) {
	discharger := newTestIdentityServer()
	defer discharger.Close()

	key, err := bakery.GenerateKey()
	if err != nil {
		panic(err)
	}
	b := bakery.New(bakery.BakeryParams{
		Location:       "here",
		Locator:        discharger,
		Key:            key,
		Checker:        httpbakery.NewChecker(),
		IdentityClient: discharger,
	})
	expectedExpiry := time.Hour
	oven := &httpbakery.Oven{
		Oven:        b.Oven,
		AuthnExpiry: expectedExpiry,
		AuthzExpiry: 5 * time.Minute,
	}
	errorCalled := 0
	handler := httpReqServer.HandleErrors(func(p httprequest.Params) error {
		if _, err := b.Checker.Auth(httpbakery.RequestMacaroons(p.Request)...).Allow(p.Context, bakery.LoginOp); err != nil {
			errorCalled++
			return oven.Error(testContext, p.Request, err)
		}
		fmt.Fprintf(p.Response, "done")
		return nil
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		handler(w, req, nil)
	}))
	defer ts.Close()
	req, err := http.NewRequest("GET", ts.URL, nil)
	c.Assert(err, gc.Equals, nil)
	client := httpbakery.NewClient()
	t0 := time.Now()
	resp, err := client.Do(req)
	c.Assert(err, gc.Equals, nil)
	c.Check(errorCalled, gc.Equals, 1)
	body, _ := ioutil.ReadAll(resp.Body)
	c.Assert(resp.StatusCode, gc.Equals, http.StatusOK, gc.Commentf("body: %q", body))
	mss := httpbakery.MacaroonsForURL(client.Jar, mustParseURL(discharger.Location()))
	c.Assert(mss, gc.HasLen, 1)
	t, ok := checkers.MacaroonsExpiryTime(b.Checker.Namespace(), mss[0])
	c.Assert(ok, gc.Equals, true)
	want := t0.Add(expectedExpiry)
	c.Assert(t, jc.TimeBetween(want, want.Add(time.Second)))
}

func (*OvenSuite) TestOvenWithAuthzMacaroon(c *gc.C) {
	discharger := newTestIdentityServer()
	defer discharger.Close()
	discharger2 := bakerytest.NewDischarger(nil)
	defer discharger2.Close()

	locator := httpbakery.NewThirdPartyLocator(nil, nil)
	locator.AllowInsecure()

	key, err := bakery.GenerateKey()
	if err != nil {
		panic(err)
	}
	b := bakery.New(bakery.BakeryParams{
		Location:       "here",
		Locator:        locator,
		Key:            key,
		Checker:        httpbakery.NewChecker(),
		IdentityClient: discharger,
		Authorizer: bakery.AuthorizerFunc(func(ctx context.Context, id bakery.Identity, op bakery.Op) (bool, []checkers.Caveat, error) {
			return true, []checkers.Caveat{{
				Location:  discharger2.Location(),
				Condition: "something",
			}}, nil
		}),
	})
	expectedAuthnExpiry := 5 * time.Minute
	expectedAuthzExpiry := time.Hour
	oven := &httpbakery.Oven{
		Oven:        b.Oven,
		AuthnExpiry: expectedAuthnExpiry,
		AuthzExpiry: expectedAuthzExpiry,
	}
	errorCalled := 0
	handler := httpReqServer.HandleErrors(func(p httprequest.Params) error {
		if _, err := b.Checker.Auth(httpbakery.RequestMacaroons(p.Request)...).Allow(p.Context, bakery.Op{"something", "read"}); err != nil {
			errorCalled++
			return oven.Error(testContext, p.Request, err)
		}
		fmt.Fprintf(p.Response, "done")
		return nil
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		handler(w, req, nil)
	}))
	defer ts.Close()
	req, err := http.NewRequest("GET", ts.URL, nil)
	c.Assert(err, gc.Equals, nil)
	client := httpbakery.NewClient()
	t0 := time.Now()
	resp, err := client.Do(req)
	c.Assert(err, gc.Equals, nil)
	c.Check(errorCalled, gc.Equals, 2)
	body, _ := ioutil.ReadAll(resp.Body)
	c.Assert(resp.StatusCode, gc.Equals, http.StatusOK, gc.Commentf("body: %q", body))

	mss := httpbakery.MacaroonsForURL(client.Jar, mustParseURL(discharger.Location()))
	c.Assert(mss, gc.HasLen, 2)

	// The cookie jar returns otherwise-similar cookies in the order
	// they were added, so the authn macaroon will be first.
	t, ok := checkers.MacaroonsExpiryTime(b.Checker.Namespace(), mss[0])
	c.Assert(ok, gc.Equals, true)
	want := t0.Add(expectedAuthnExpiry)
	c.Assert(t, jc.TimeBetween(want, want.Add(time.Second)))

	t, ok = checkers.MacaroonsExpiryTime(b.Checker.Namespace(), mss[1])
	c.Assert(ok, gc.Equals, true)
	want = t0.Add(expectedAuthzExpiry)
	c.Assert(t, jc.TimeBetween(want, want.Add(time.Second)))
}

type testIdentityServer struct {
	*bakerytest.Discharger
}

func newTestIdentityServer() *testIdentityServer {
	checker := func(ctx context.Context, req *http.Request, cav *bakery.ThirdPartyCaveatInfo, token *httpbakery.DischargeToken) ([]checkers.Caveat, error) {
		if string(cav.Condition) != "is-authenticated-user" {
			return nil, errgo.New("unexpected caveat")
		}
		return []checkers.Caveat{
			checkers.DeclaredCaveat("username", "bob"),
		}, nil
	}
	discharger := bakerytest.NewDischarger(nil)
	discharger.Checker = httpbakery.ThirdPartyCaveatCheckerFunc(checker)
	return &testIdentityServer{
		Discharger: discharger,
	}
}

func (s *testIdentityServer) IdentityFromContext(ctx context.Context) (bakery.Identity, []checkers.Caveat, error) {
	return nil, []checkers.Caveat{{
		Location:  s.Location(),
		Condition: "is-authenticated-user",
	}}, nil
}

func (s *testIdentityServer) DeclaredIdentity(ctx context.Context, declared map[string]string) (bakery.Identity, error) {
	username, ok := declared["username"]
	if !ok {
		return nil, errgo.New("no username declared")
	}
	return bakery.SimpleIdentity(username), nil
}
