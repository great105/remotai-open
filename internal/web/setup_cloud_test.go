package web

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tgcontrol/internal/relay"
)

func TestCloudPairAccountURLSurvivesLogin(t *testing.T) {
	u, err := url.Parse(buildCloudPairAccountURL("https://remotai.ru/", "AB12-CD34"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "remotai.ru" || u.Path != "/app/" || u.RawQuery != "" {
		t.Fatalf("account link must keep code out of the HTTP request: %s", u)
	}
	route, query, ok := strings.Cut(u.EscapedFragment(), "?")
	if !ok || route != "/cloud-login" {
		t.Fatalf("missing account login: %s", u.Fragment)
	}
	loginQuery, _ := url.ParseQuery(query)
	next, err := url.Parse(loginQuery.Get("next"))
	if err != nil || next.Path != "/infrastructure" || next.Query().Get("add") != "1" || next.Query().Get("code") != "AB12-CD34" {
		t.Fatalf("code/confirmation route lost across login: %s", loginQuery.Get("next"))
	}
}

func TestCloudPairAccountURLRejectsInvalidRelay(t *testing.T) {
	for _, base := range []string{"javascript:alert(1)", "file:///tmp/app", "https://user:password@example.test"} {
		if got := buildCloudPairAccountURL(base, "AB12-CD34"); got != "" {
			t.Fatalf("unexpected account URL for invalid relay: %s", got)
		}
	}
}

func TestWaitCloudPairingRetriesUntilConfirmed(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	status, err := waitCloudPairing(ctx, time.Millisecond, func(context.Context) (*relay.PairingStatus, error) {
		switch calls.Add(1) {
		case 1:
			return &relay.PairingStatus{}, nil
		case 2:
			return nil, errors.New("temporary relay failure")
		default:
			return &relay.PairingStatus{Confirmed: true, JWT: "issued"}, nil
		}
	})
	if err != nil {
		t.Fatalf("waitCloudPairing: %v", err)
	}
	if status == nil || !status.Confirmed || status.JWT != "issued" {
		t.Fatalf("unexpected status: %#v", status)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("calls = %d, want 3", got)
	}
}

func TestWaitCloudPairingStopsWhenExpired(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	status, err := waitCloudPairing(ctx, time.Millisecond, func(context.Context) (*relay.PairingStatus, error) {
		return &relay.PairingStatus{Expired: true}, nil
	})
	if err != nil {
		t.Fatalf("waitCloudPairing: %v", err)
	}
	if status == nil || !status.Expired {
		t.Fatalf("unexpected status: %#v", status)
	}
}

func TestWaitCloudPairingHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	status, err := waitCloudPairing(ctx, time.Millisecond, func(context.Context) (*relay.PairingStatus, error) {
		t.Fatal("check must not run after cancellation")
		return nil, nil
	})
	if status != nil {
		t.Fatalf("status = %#v, want nil", status)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
