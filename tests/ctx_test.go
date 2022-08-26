package tests

import (
	"context"
	"fmt"
	"net/smtp"
	"testing"

	"github.com/Masterminds/semver/v3"
	"github.com/ProtonMail/proton-bridge/v2/internal/bridge"
	"github.com/ProtonMail/proton-bridge/v2/internal/events"
	"github.com/ProtonMail/proton-bridge/v2/internal/locations"
	"github.com/bradenaw/juniper/xslices"
	"github.com/emersion/go-imap/client"
	"gitlab.protontech.ch/go/liteapi"
	"gitlab.protontech.ch/go/liteapi/server"
)

var defaultVersion = semver.MustParse("1.0.0")

type testCtx struct {
	// These are the objects supporting the test.
	dir      string
	api      API
	locator  *locations.Locations
	storeKey []byte
	version  *semver.Version
	mocks    *bridge.Mocks

	// bridge holds the bridge app under test.
	bridge *bridge.Bridge

	// These channels hold events of various types coming from bridge.
	connStatusCh   <-chan events.ConnStatus
	userLoginCh    <-chan events.UserLoggedIn
	userLogoutCh   <-chan events.UserLoggedOut
	userDeletedCh  <-chan events.UserDeleted
	userDeauthCh   <-chan events.UserDeauth
	syncStartedCh  <-chan events.SyncStarted
	syncFinishedCh <-chan events.SyncFinished
	forcedUpdateCh <-chan events.UpdateForced
	updateCh       <-chan events.Event

	// These maps hold expected userIDByName, their primary addresses and bridge passwords.
	userIDByName map[string]string
	userAddrByID map[string]string
	userPassByID map[string]string
	addrIDByID   map[string]string

	// These are the IMAP and SMTP clients used to connect to bridge.
	imapClients map[string]*imapClient
	smtpClients map[string]*smtpClient

	// calls holds calls made to the API during each step of the test.
	calls [][]server.Call

	// errors holds test-related errors encountered while running test steps.
	errors [][]error
}

type imapClient struct {
	userID string
	client *client.Client
}

type smtpClient struct {
	userID string
	client *smtp.Client
}

func newTestCtx(tb testing.TB) *testCtx {
	ctx := &testCtx{
		dir:      tb.TempDir(),
		api:      newFakeAPI(),
		locator:  locations.New(bridge.NewTestLocationsProvider(tb), "config-name"),
		storeKey: []byte("super-secret-store-key"),
		mocks:    bridge.NewMocks(tb, defaultVersion, defaultVersion),
		version:  defaultVersion,

		userIDByName: make(map[string]string),
		userAddrByID: make(map[string]string),
		userPassByID: make(map[string]string),
		addrIDByID:   make(map[string]string),

		imapClients: make(map[string]*imapClient),
		smtpClients: make(map[string]*smtpClient),
	}

	ctx.api.AddCallWatcher(func(call server.Call) {
		ctx.calls[len(ctx.calls)-1] = append(ctx.calls[len(ctx.calls)-1], call)
	})

	return ctx
}

func (t *testCtx) beforeStep() {
	t.calls = append(t.calls, nil)
	t.errors = append(t.errors, nil)
}

func (t *testCtx) getUserID(username string) string {
	return t.userIDByName[username]
}

func (t *testCtx) setUserID(username, userID string) {
	t.userIDByName[username] = userID
}

func (t *testCtx) getUserAddr(userID string) string {
	return t.userAddrByID[userID]
}

func (t *testCtx) setUserAddr(userID, addr string) {
	t.userAddrByID[userID] = addr
}

func (t *testCtx) getUserPass(userID string) string {
	return t.userPassByID[userID]
}

func (t *testCtx) setUserPass(userID, pass string) {
	t.userPassByID[userID] = pass
}

func (t *testCtx) getAddrID(userID string) string {
	return t.addrIDByID[userID]
}

func (t *testCtx) setAddrID(userID, addrID string) {
	t.addrIDByID[userID] = addrID
}

func (t *testCtx) getMBoxID(userID string, name string) string {
	labels, err := t.api.GetLabels(userID)
	if err != nil {
		panic(err)
	}

	idx := xslices.IndexFunc(labels, func(label liteapi.Label) bool {
		return label.Name == name
	})

	if idx < 0 {
		panic(fmt.Errorf("label %q not found", name))
	}

	return labels[idx].ID
}

func (t *testCtx) getLastCall(path string) (server.Call, error) {
	calls := t.calls[len(t.calls)-2]

	if len(calls) == 0 {
		return server.Call{}, fmt.Errorf("no calls made")
	}

	for _, call := range calls {
		if call.URL.Path == path {
			return call, nil
		}
	}

	return calls[len(calls)-1], nil
}

func (t *testCtx) pushError(err error) {
	t.errors[len(t.errors)-1] = append(t.errors[len(t.errors)-1], err)
}

func (t *testCtx) getLastError() error {
	errors := t.errors[len(t.errors)-2]

	if len(errors) == 0 {
		return nil
	}

	return errors[len(errors)-1]
}

func (t *testCtx) close(ctx context.Context) error {
	for _, client := range t.imapClients {
		if err := client.client.Logout(); err != nil {
			return err
		}
	}

	if t.bridge != nil {
		if err := t.bridge.Close(ctx); err != nil {
			return err
		}
	}

	t.api.Close()

	return nil
}

func chToType[In, Out any](inCh <-chan In, done any) <-chan Out {
	outCh := make(chan Out)

	go func() {
		defer close(outCh)

		for in := range inCh {
			outCh <- any(in).(Out)
		}
	}()

	return outCh
}