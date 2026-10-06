package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/hookflow"
)

// TestDoctorOmitsDeliveryDropsWhenNoStatusFileExists is the common case: a
// machine that has never run a lane daemon under this build (or ran an older
// binary that never persisted one) must see no new section at all.
func TestDoctorOmitsDeliveryDropsWhenNoStatusFileExists(t *testing.T) {
	isolateHome(t)
	a, out, errb := testApp(nil)
	if code := a.runDoctor(nil); code != exitOK && code != exitError {
		t.Fatalf("doctor exit = %d; stderr=%q", code, errb.String())
	}
	if strings.Contains(out.String(), "Delivery (lane records queue through each tool's session spool") {
		t.Errorf("doctor reported a delivery-drops section with no status file on disk:\n%s", out.String())
	}
}

// TestDoctorReportsADeliveryDropsRowWhenAStatusFileExists is the disclosure
// surface a daemon that sends in-process, with no spool behind it, needs: a
// lane daemon persists its DeliverPool.Dropped() count to a small status file
// (hookflow.StatusPersister), and `doctor`, running as a separate process,
// must read it back and print it -- no IPC, no port.
func TestDoctorReportsADeliveryDropsRowWhenAStatusFileExists(t *testing.T) {
	home := isolateHome(t)
	since := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	writeDeliveryStatus(t, home, "telemetry", hookflow.DeliverStatus{Dropped: 7, Since: since})

	a, out, errb := testApp(nil)
	if code := a.runDoctor(nil); code != exitOK && code != exitError {
		t.Fatalf("doctor exit = %d; stderr=%q", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "Delivery (lane records queue through each tool's session spool") {
		t.Fatalf("doctor did not report the delivery-drops section with a status file present:\n%s", s)
	}
	if !strings.Contains(s, "telemetry") || !strings.Contains(s, "7 record(s) not accepted since") {
		t.Errorf("doctor did not report telemetry's dropped count:\n%s", s)
	}
	if strings.Contains(s, "transport") && strings.Contains(s, "0 record(s) not accepted") {
		t.Errorf("doctor invented a transport row with no status file for it:\n%s", s)
	}
}

// TestDoctorReportsBothLanesIndependently proves the two lanes never share
// one file or one row: each daemon persists (and doctor reads) its own.
func TestDoctorReportsBothLanesIndependently(t *testing.T) {
	home := isolateHome(t)
	writeDeliveryStatus(t, home, "telemetry", hookflow.DeliverStatus{Dropped: 2, Since: time.Now()})
	writeDeliveryStatus(t, home, "transport", hookflow.DeliverStatus{Dropped: 9, Since: time.Now()})

	a, out, errb := testApp(nil)
	if code := a.runDoctor(nil); code != exitOK && code != exitError {
		t.Fatalf("doctor exit = %d; stderr=%q", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "2 record(s) not accepted since") {
		t.Errorf("doctor did not report telemetry's own count:\n%s", s)
	}
	if !strings.Contains(s, "9 record(s) not accepted since") {
		t.Errorf("doctor did not report transport's own count:\n%s", s)
	}
}

// TestDoctorTreatsAnUnparsableStatusFileAsAbsent is the fail-open direction a
// reporting-only surface must take: a corrupt status file must not error
// doctor out or crash it, and must render as if it were not there.
func TestDoctorTreatsAnUnparsableStatusFileAsAbsent(t *testing.T) {
	home := isolateHome(t)
	if err := os.WriteFile(hookflow.DeliverStatusPath(home, "telemetry"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	a, out, errb := testApp(nil)
	if code := a.runDoctor(nil); code != exitOK && code != exitError {
		t.Fatalf("doctor exit = %d; stderr=%q", code, errb.String())
	}
	if strings.Contains(out.String(), "Delivery (lane records queue through each tool's session spool") {
		t.Errorf("doctor reported a delivery-drops section from an unparsable status file:\n%s", out.String())
	}
}

func writeDeliveryStatus(t *testing.T, home, lane string, status hookflow.DeliverStatus) {
	t.Helper()
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hookflow.DeliverStatusPath(home, lane), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
