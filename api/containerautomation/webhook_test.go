package containerautomation

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/datastore"
)

// newTestWebhookNotifier builds an initialized test datastore, sets both the
// update and heal webhook URLs to the same value (so the notifier fires for
// every event kind), and returns a webhookNotifier bound to it. Use
// newTestWebhookNotifierSplit to configure the two URLs independently.
func newTestWebhookNotifier(t *testing.T, webhookURL string) (webhookNotifier, *datastore.Store) {
	t.Helper()

	return newTestWebhookNotifierSplit(t, webhookURL, webhookURL)
}

// newTestWebhookNotifierSplit builds an initialized test datastore with the
// auto-update and auto-heal webhook URLs set independently, and returns a
// webhookNotifier bound to it.
func newTestWebhookNotifierSplit(t *testing.T, updateURL, healURL string) (webhookNotifier, *datastore.Store) {
	t.Helper()

	_, store := datastore.MustNewTestStore(t, true, false)

	settings, err := store.Settings().Settings()
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}

	settings.ContainerAutomation.Notification.UpdateWebhookURL = updateURL
	settings.ContainerAutomation.Notification.HealWebhookURL = healURL
	if err := store.Settings().UpdateSettings(settings); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	return newWebhookNotifier(store), store
}

func createEndpoint(t *testing.T, store *datastore.Store, id int, name string) {
	t.Helper()

	if err := store.Endpoint().Create(&portainer.Endpoint{ID: portainer.EndpointID(id), Name: name}); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
}

func createStack(t *testing.T, store *datastore.Store, id int, name string) {
	t.Helper()

	if err := store.Stack().Create(&portainer.Stack{ID: portainer.StackID(id), Name: name}); err != nil {
		t.Fatalf("create stack: %v", err)
	}
}

// TestWebhookNotifierGETPlaceholder verifies the placeholder is replaced with the
// URL-encoded message and the URL is fetched with GET.
func TestWebhookNotifierGETPlaceholder(t *testing.T) {
	reqs := make(chan *http.Request, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs <- r
	}))
	defer srv.Close()

	n, store := newTestWebhookNotifier(t, srv.URL+"/hook?msg="+webhookMessagePlaceholder)
	createEndpoint(t, store, 1, "prod")

	n.Notify(Event{Kind: EventHealRestarted, EndpointID: 1, ContainerName: "nginx"})

	select {
	case r := <-reqs:
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}

		got := r.URL.Query().Get("msg")
		want := "Environment | prod\nContainer [nginx]\nAuto-heal: restarted unhealthy container"
		if got != want {
			t.Errorf("decoded msg = %q, want %q", got, want)
		}

		// The raw query must be URL-encoded: no literal spaces/newlines on the wire.
		if strings.ContainsAny(r.URL.RawQuery, " \n") {
			t.Errorf("raw query is not URL-encoded: %q", r.URL.RawQuery)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook GET was not received")
	}
}

// TestWebhookNotifierPOSTFallback verifies that a URL without the placeholder is
// POSTed with the plain-text message as the body.
func TestWebhookNotifierPOSTFallback(t *testing.T) {
	type captured struct {
		method string
		body   string
	}

	ch := make(chan captured, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- captured{method: r.Method, body: string(b)}
	}))
	defer srv.Close()

	n, store := newTestWebhookNotifier(t, srv.URL+"/hook")
	createEndpoint(t, store, 2, "staging")

	n.Notify(Event{Kind: EventHealRestarted, EndpointID: 2, ContainerName: "api"})

	select {
	case c := <-ch:
		if c.method != http.MethodPost {
			t.Errorf("method = %s, want POST", c.method)
		}

		want := "Environment | staging\nContainer [api]\nAuto-heal: restarted unhealthy container"
		if c.body != want {
			t.Errorf("body = %q, want %q", c.body, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook POST was not received")
	}
}

// TestWebhookNotifierEmptyURLNoCall verifies no HTTP call is made when the URL is
// empty.
func TestWebhookNotifierEmptyURLNoCall(t *testing.T) {
	called := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called <- struct{}{}
	}))
	defer srv.Close()

	n, _ := newTestWebhookNotifier(t, "")
	n.Notify(Event{Kind: EventHealRestarted, EndpointID: 1, ContainerName: "x"})

	select {
	case <-called:
		t.Fatal("webhook should not be called when the URL is empty")
	case <-time.After(300 * time.Millisecond):
		// No call, as expected.
	}
}

// waitForRequest returns the first request seen on ch, or fails after a short
// grace period.
func waitForRequest(t *testing.T, ch <-chan *http.Request, what string) *http.Request {
	t.Helper()

	select {
	case r := <-ch:
		return r
	case <-time.After(2 * time.Second):
		t.Fatalf("%s was not received", what)
		return nil
	}
}

// expectNoRequest asserts nothing arrives on ch within a short grace period.
func expectNoRequest(t *testing.T, ch <-chan *http.Request, what string) {
	t.Helper()

	select {
	case <-ch:
		t.Fatalf("%s should not have been called", what)
	case <-time.After(300 * time.Millisecond):
		// No call, as expected.
	}
}

// TestWebhookNotifierUpdateEventRoutesToUpdateURL verifies an update-family event
// dispatches to the auto-update URL only; the heal URL is set but never called.
func TestWebhookNotifierUpdateEventRoutesToUpdateURL(t *testing.T) {
	updateReqs := make(chan *http.Request, 1)
	updateSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		updateReqs <- r
	}))
	defer updateSrv.Close()

	healReqs := make(chan *http.Request, 1)
	healSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healReqs <- r
	}))
	defer healSrv.Close()

	n, store := newTestWebhookNotifierSplit(t, updateSrv.URL+"/update", healSrv.URL+"/heal")
	createEndpoint(t, store, 1, "prod")

	for _, kind := range []EventKind{EventUpdated, EventRollback, EventUpdateFailed} {
		n.Notify(Event{Kind: kind, EndpointID: 1, ContainerName: "c"})

		r := waitForRequest(t, updateReqs, "update webhook for "+string(kind))
		if r.URL.Path != "/update" {
			t.Errorf("kind %s hit %q, want /update", kind, r.URL.Path)
		}
	}

	expectNoRequest(t, healReqs, "heal webhook")
}

// TestWebhookNotifierHealEventRoutesToHealURL verifies a heal event dispatches to
// the auto-heal URL only; the update URL is set but never called.
func TestWebhookNotifierHealEventRoutesToHealURL(t *testing.T) {
	updateReqs := make(chan *http.Request, 1)
	updateSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		updateReqs <- r
	}))
	defer updateSrv.Close()

	healReqs := make(chan *http.Request, 1)
	healSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healReqs <- r
	}))
	defer healSrv.Close()

	n, store := newTestWebhookNotifierSplit(t, updateSrv.URL+"/update", healSrv.URL+"/heal")
	createEndpoint(t, store, 1, "prod")

	n.Notify(Event{Kind: EventHealRestarted, EndpointID: 1, ContainerName: "nginx"})

	r := waitForRequest(t, healReqs, "heal webhook")
	if r.URL.Path != "/heal" {
		t.Errorf("heal event hit %q, want /heal", r.URL.Path)
	}

	expectNoRequest(t, updateReqs, "update webhook")
}

// TestWebhookNotifierEmptyUpdateURLSkipsUpdateOnly verifies that an empty
// auto-update URL suppresses update-family events while heal still fires.
func TestWebhookNotifierEmptyUpdateURLSkipsUpdateOnly(t *testing.T) {
	healReqs := make(chan *http.Request, 1)
	healSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healReqs <- r
	}))
	defer healSrv.Close()

	n, store := newTestWebhookNotifierSplit(t, "", healSrv.URL+"/heal")
	createEndpoint(t, store, 1, "prod")

	// Update-family event: no URL configured, so nothing is delivered.
	n.Notify(Event{Kind: EventUpdated, EndpointID: 1, ContainerName: "c"})
	expectNoRequest(t, healReqs, "heal webhook on an update event")

	// Heal event: the heal URL is set, so it still fires.
	n.Notify(Event{Kind: EventHealRestarted, EndpointID: 1, ContainerName: "nginx"})
	waitForRequest(t, healReqs, "heal webhook")
}

// TestWebhookNotifierEmptyHealURLSkipsHealOnly verifies that an empty auto-heal
// URL suppresses heal events while update-family events still fire.
func TestWebhookNotifierEmptyHealURLSkipsHealOnly(t *testing.T) {
	updateReqs := make(chan *http.Request, 1)
	updateSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		updateReqs <- r
	}))
	defer updateSrv.Close()

	n, store := newTestWebhookNotifierSplit(t, updateSrv.URL+"/update", "")
	createEndpoint(t, store, 1, "prod")

	// Heal event: no URL configured, so nothing is delivered.
	n.Notify(Event{Kind: EventHealRestarted, EndpointID: 1, ContainerName: "nginx"})
	expectNoRequest(t, updateReqs, "update webhook on a heal event")

	// Update event: the update URL is set, so it still fires.
	n.Notify(Event{Kind: EventUpdated, EndpointID: 1, ContainerName: "c"})
	waitForRequest(t, updateReqs, "update webhook")
}

// TestWebhookNotifierFailingEndpointDoesNotBlock verifies that a broken endpoint
// neither blocks the caller nor panics.
func TestWebhookNotifierFailingEndpointDoesNotBlock(t *testing.T) {
	// Start then immediately close a server so its address refuses connections.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close()

	n, store := newTestWebhookNotifier(t, deadURL+"/hook?msg="+webhookMessagePlaceholder)
	createEndpoint(t, store, 1, "prod")

	done := make(chan struct{})
	go func() {
		n.Notify(Event{Kind: EventUpdated, EndpointID: 1, ContainerName: "c"})
		close(done)
	}()

	select {
	case <-done:
		// Notify returned promptly despite the failing endpoint.
	case <-time.After(1 * time.Second):
		t.Fatal("Notify blocked on a failing endpoint")
	}

	// Give the background delivery goroutine time to hit the error path; it must
	// log-and-return, never panic.
	time.Sleep(200 * time.Millisecond)
}

// TestFormatMessageStandaloneUpdate covers the maintainer's update format for a
// standalone container, with the old->new short digests.
func TestFormatMessageStandaloneUpdate(t *testing.T) {
	n, store := newTestWebhookNotifier(t, "unused")
	createEndpoint(t, store, 1, "nebula.lc")

	settings, _ := store.Settings().Settings()

	msg := n.formatMessage(settings, Event{
		Kind: EventUpdated, EndpointID: 1, ContainerName: "esphome",
		OldDigest: "sha256:59b94983c73aabcd", NewDigest: "sha256:2231ca5d676dabcd",
	})

	want := "Environment | nebula.lc\nContainer [esphome]\nUpdate [esphome]: 59b94983c73a → 2231ca5d676d"
	if msg != want {
		t.Errorf("got:\n%q\nwant:\n%q", msg, want)
	}
}

// TestFormatMessageStackUpdate covers a stack-scoped update (no per-container
// digests): the context line is the stack name.
func TestFormatMessageStackUpdate(t *testing.T) {
	n, store := newTestWebhookNotifier(t, "unused")
	createEndpoint(t, store, 1, "nebula.lc")
	createStack(t, store, 7, "cache-demo")

	settings, _ := store.Settings().Settings()

	msg := n.formatMessage(settings, Event{
		Kind: EventUpdated, EndpointID: 1, StackID: 7,
	})

	want := "Environment | nebula.lc\nStack [cache-demo]\nUpdate [cache-demo]: image updated"
	if msg != want {
		t.Errorf("got:\n%q\nwant:\n%q", msg, want)
	}
}

// TestFormatMessageStackMemberUpdate covers the per-container update of a
// stack-member container: the context line is the compose stack name (from
// StackName, no Stack().Read), the action line names the container with its
// old->new digests. This is the maintainer's target output.
func TestFormatMessageStackMemberUpdate(t *testing.T) {
	n, store := newTestWebhookNotifier(t, "unused")
	createEndpoint(t, store, 1, "nebula.lc")

	settings, _ := store.Settings().Settings()

	msg := n.formatMessage(settings, Event{
		Kind: EventUpdated, EndpointID: 1, StackID: 7, StackName: "cache-demo",
		ContainerName: "esphome",
		OldDigest:     "sha256:59b94983c73aabcd", NewDigest: "sha256:2231ca5d676dabcd",
	})

	want := "Environment | nebula.lc\nStack [cache-demo]\nUpdate [esphome]: 59b94983c73a → 2231ca5d676d"
	if msg != want {
		t.Errorf("got:\n%q\nwant:\n%q", msg, want)
	}
}

// TestFormatMessageStackMemberUpdateNoNewDigest covers the best-effort fallback:
// when the post-redeploy new image id could not be recovered, the message still
// carries the stack and container and degrades the action line to "image updated"
// rather than blocking delivery.
func TestFormatMessageStackMemberUpdateNoNewDigest(t *testing.T) {
	n, store := newTestWebhookNotifier(t, "unused")
	createEndpoint(t, store, 1, "nebula.lc")

	settings, _ := store.Settings().Settings()

	msg := n.formatMessage(settings, Event{
		Kind: EventUpdated, EndpointID: 1, StackID: 7, StackName: "cache-demo",
		ContainerName: "esphome", OldDigest: "sha256:59b94983c73aabcd",
	})

	want := "Environment | nebula.lc\nStack [cache-demo]\nUpdate [esphome]: image updated"
	if msg != want {
		t.Errorf("got:\n%q\nwant:\n%q", msg, want)
	}
}

// TestFormatMessageAutoHeal covers the auto-heal message design.
func TestFormatMessageAutoHeal(t *testing.T) {
	n, store := newTestWebhookNotifier(t, "unused")
	createEndpoint(t, store, 3, "prod")

	settings, _ := store.Settings().Settings()

	msg := n.formatMessage(settings, Event{
		Kind: EventHealRestarted, EndpointID: 3, ContainerName: "nginx",
	})

	want := "Environment | prod\nContainer [nginx]\nAuto-heal: restarted unhealthy container"
	if msg != want {
		t.Errorf("got:\n%q\nwant:\n%q", msg, want)
	}
}

// TestFormatMessageUnknownEndpoint verifies the "#<id>" fallback when the
// endpoint cannot be resolved.
func TestFormatMessageUnknownEndpoint(t *testing.T) {
	n, store := newTestWebhookNotifier(t, "unused")

	settings, _ := store.Settings().Settings()

	msg := n.formatMessage(settings, Event{
		Kind: EventHealRestarted, EndpointID: 99, ContainerName: "ghost",
	})

	want := "Environment | #99\nContainer [ghost]\nAuto-heal: restarted unhealthy container"
	if msg != want {
		t.Errorf("got:\n%q\nwant:\n%q", msg, want)
	}
}

// TestShortDigest covers digest short-forming.
func TestShortDigest(t *testing.T) {
	cases := map[string]string{
		"sha256:59b94983c73a1122334455": "59b94983c73a",
		"59b94983c73a1122334455":        "59b94983c73a",
		"short":                         "short",
		"":                              "",
	}

	for in, want := range cases {
		if got := shortDigest(in); got != want {
			t.Errorf("shortDigest(%q) = %q, want %q", in, got, want)
		}
	}
}
