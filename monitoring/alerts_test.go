package main

import "testing"

// alerts.json is what Beszel emails you about; report.go's ACT constants are
// what the weekly report calls "act". They are meant to be the same line, so
// an alert means "act" and the weekly email covers the slower WATCH drift.
// This is where that stops being a convention and becomes a check: change a
// threshold in one place and this fails until the other matches.
func TestAlertsMatchReportActLines(t *testing.T) {
	specs, err := loadAlerts()
	if err != nil {
		t.Fatal(err)
	}
	// The J4125's thread count. Load is an absolute number in Beszel's alert
	// but per-thread in the report, so the two meet at this host's size.
	const threads = 4
	want := map[string]float64{
		"Memory":      memAvgAct,
		"CPU":         cpuAvgAct,
		"CPUIOWait":   iowaitAct,
		"LoadAvg15":   loadActPerThread * threads,
		"Disk":        diskAct,
		"Temperature": tempAct,
	}
	got := map[string]alertSpec{}
	for _, s := range specs {
		got[s.Name] = s
	}
	for name, v := range want {
		s, ok := got[name]
		if !ok {
			t.Errorf("alerts.json has no %s alert; the report has an ACT line for it", name)
			continue
		}
		if s.Value != v {
			t.Errorf("%s: alerts.json says %g, report.go's ACT line is %g", name, s.Value, v)
		}
	}
	for _, name := range []string{"Status", "ContainerHealth"} {
		if _, ok := got[name]; !ok {
			t.Errorf("alerts.json is missing %s, which no report line replaces", name)
		}
	}
	for _, s := range specs {
		if s.Min == 0 {
			t.Errorf("%s: min 0 fires on a single sample; give it a duration", s.Name)
		}
	}
}

func TestLoadAlertsRejectsUnknownAndDuplicateNames(t *testing.T) {
	saved := alertsJSON
	defer func() { alertsJSON = saved }()
	for _, bad := range []string{
		`[{"name":"Cpu","value":70,"min":60}]`,
		`[{"name":"CPU","value":70,"min":60},{"name":"CPU","value":80,"min":60}]`,
		`not json`,
	} {
		alertsJSON = []byte(bad)
		if _, err := loadAlerts(); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
	alertsJSON = []byte(`[{"name":"CPU","value":70,"min":60}]`)
	if specs, err := loadAlerts(); err != nil || len(specs) != 1 {
		t.Errorf("valid file rejected: %v %v", specs, err)
	}
}
