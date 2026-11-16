package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeMail records what a scheduled run sent and whether it pinged. A subject
// that starts with failSubject is refused, as SMTP would.
type fakeMail struct {
	sent        []string
	pings       int
	pauses      int
	failSubject string
}

func (f *fakeMail) mailer() mailer {
	return mailer{
		send: func(m email) error {
			if f.failSubject != "" && strings.HasPrefix(m.Subject, f.failSubject) {
				return errors.New("smtp: refused")
			}
			f.sent = append(f.sent, m.Subject)
			return nil
		},
		ping:  func() { f.pings++ },
		pause: func(context.Context) bool { f.pauses++; return true },
	}
}

func scheduled(t *testing.T, hh hostHub, f *fakeMail) {
	t.Helper()
	cfg := config{Email: "r@example.com", Password: "pw", System: "kyles-nas", PingURL: "http://ping.invalid"}
	cfg.BeszelURL = hh.url(t)
	runScheduled(context.Background(), cfg, time.Now(), f.mailer())
}

func countPrefix(subjects []string, prefix string) int {
	n := 0
	for _, s := range subjects {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

func TestScheduledSendsBothAndPings(t *testing.T) {
	f := &fakeMail{}
	scheduled(t, hostHub{names: []string{"kyles-nas", "mac-studio"}}, f)
	if len(f.sent) != 2 || f.pings != 1 || f.pauses != 0 {
		t.Fatalf("sent %v, %d pings, %d pauses", f.sent, f.pings, f.pauses)
	}
}

// The mac studio failing every attempt: the NAS's email goes once, the
// failure email carries the mac studio's subject, and there is no ping.
func TestScheduledMacFailureSparesTheNAS(t *testing.T) {
	f := &fakeMail{}
	scheduled(t, hostHub{names: []string{"kyles-nas", "mac-studio"}, broken: "mac-studio"}, f)
	if countPrefix(f.sent, "[homelab] weekly") != 1 || strings.Contains(strings.Join(f.sent, "|"), "[homelab] weekly: report failed") {
		t.Fatalf("NAS emails: %v", f.sent)
	}
	if countPrefix(f.sent, "[homelab] mac studio weekly: report failed") != 1 || len(f.sent) != 2 {
		t.Fatalf("sent %v, want the NAS's report and one mac studio failure", f.sent)
	}
	if f.pings != 0 || f.pauses != 2 {
		t.Fatalf("%d pings, %d pauses", f.pings, f.pauses)
	}
}

// The hub never answering: one failure email, under the NAS's subject, since
// the hub cannot say whether it has the mac studio.
func TestScheduledHubDownSendsOneFailure(t *testing.T) {
	f := &fakeMail{}
	scheduled(t, hostHub{names: []string{"kyles-nas", "mac-studio"}, failLogin: true}, f)
	if len(f.sent) != 1 || f.sent[0] != "[homelab] weekly: report failed" || f.pings != 0 {
		t.Fatalf("sent %v, %d pings", f.sent, f.pings)
	}
}

// One send refused: the other email still goes, and the week does not ping.
func TestScheduledPartialSendDoesNotPing(t *testing.T) {
	f := &fakeMail{failSubject: "[homelab] weekly"}
	scheduled(t, hostHub{names: []string{"kyles-nas", "mac-studio"}}, f)
	if len(f.sent) != 1 || !strings.HasPrefix(f.sent[0], "[homelab] mac studio weekly") || f.pings != 0 {
		t.Fatalf("sent %v, %d pings", f.sent, f.pings)
	}
}
