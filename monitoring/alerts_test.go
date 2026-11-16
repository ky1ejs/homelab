package main

import "testing"

// alerts.json is what Beszel emails you about; the ACT lines in hosts.go are
// what the weekly report calls "act". They are meant to be the same line, so
// an alert means "act" and the weekly email covers the slower WATCH drift.
// This is where that stops being a convention and becomes a check: change a
// threshold in one place and this fails until the other matches.
func TestAlertsMatchReportActLines(t *testing.T) {
	byHost, err := loadAlerts()
	if err != nil {
		t.Fatal(err)
	}
	nas := nasHost("")
	// The J4125's thread count. Load is an absolute number in Beszel's alert
	// but per-thread in the report, so the two meet at this host's size.
	const nasThreads = 4
	for _, tc := range []struct {
		key  string
		want map[string]float64
		// Alerts with no report line: the report cannot see them coming.
		extra []string
	}{
		{"nas", map[string]float64{
			"Memory":      nas.MemAvg.Act,
			"CPU":         nas.CPUAvg.Act,
			"CPUIOWait":   nas.IOWait.Act,
			"LoadAvg15":   nas.LoadPerThread.Act * nasThreads,
			"Disk":        nas.Disk.Act,
			"Temperature": nas.Temp.Act,
		}, []string{"Status", "ContainerHealth"}},
		// No CPU or load alert: on a build server a busy hour is the job, and
		// the weekly email judges the average. No memory alert: percent is
		// not what says macOS is short (hosts.go); the heartbeat alerts on
		// pressure.
		{"mac-studio", map[string]float64{
			"Disk": macStudioHost.Disk.Act,
		}, []string{"Status"}},
	} {
		got := map[string]alertSpec{}
		for _, s := range byHost[tc.key] {
			got[s.Name] = s
		}
		for name, v := range tc.want {
			s, ok := got[name]
			if !ok {
				t.Errorf("%s: alerts.json has no %s alert; the report has an ACT line for it", tc.key, name)
				continue
			}
			if s.Value != v {
				t.Errorf("%s: %s: alerts.json says %g, the report's ACT line is %g", tc.key, name, s.Value, v)
			}
		}
		for _, name := range tc.extra {
			if _, ok := got[name]; !ok {
				t.Errorf("%s: alerts.json is missing %s, which no report line replaces", tc.key, name)
			}
		}
		if len(got) != len(tc.want)+len(tc.extra) {
			t.Errorf("%s: alerts.json has %d alerts, this test expects %d: add the new one here with its reason", tc.key, len(got), len(tc.want)+len(tc.extra))
		}
		for _, s := range byHost[tc.key] {
			if s.Min == 0 {
				t.Errorf("%s: %s: min 0 fires on a single sample; give it a duration", tc.key, s.Name)
			}
		}
	}
}

func TestLoadAlertsRejectsBadFiles(t *testing.T) {
	saved := alertsJSON
	defer func() { alertsJSON = saved }()
	const mac = `"mac-studio":[{"name":"Status","value":0,"min":10}]`
	for _, bad := range []string{
		`{"nas":[{"name":"Cpu","value":70,"min":60}],` + mac + `}`,
		`{"nas":[{"name":"CPU","value":70,"min":60},{"name":"CPU","value":80,"min":60}],` + mac + `}`,
		`{"nas":[{"name":"CPU","value":70,"min":60}]}`,
		`{"nas":[{"name":"CPU","value":70,"min":60}],` + mac + `,"nass":[{"name":"CPU","value":70,"min":60}]}`,
		`[{"name":"CPU","value":70,"min":60}]`,
		`not json`,
	} {
		alertsJSON = []byte(bad)
		if _, err := loadAlerts(); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
	alertsJSON = []byte(`{"nas":[{"name":"CPU","value":70,"min":60}],` + mac + `}`)
	if byHost, err := loadAlerts(); err != nil || len(byHost["nas"]) != 1 || len(byHost["mac-studio"]) != 1 {
		t.Errorf("valid file rejected: %v %v", byHost, err)
	}
}
