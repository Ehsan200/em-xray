package daemon

import (
	"errors"
	"strings"
	"testing"
)

func TestXrayHealthFailureThreshold(t *testing.T) {
	s := &Supervisor{}
	for i := 1; i < xrayHealthFailureLimit; i++ {
		if s.recordHealthResult(errors.New("stalled")) {
			t.Fatalf("restart requested after only %d failures", i)
		}
	}
	if !s.recordHealthResult(errors.New("stalled")) {
		t.Fatal("restart not requested at failure threshold")
	}
	checked, responsive, message, _, _ := s.XrayHealth()
	if !checked || responsive || !strings.Contains(message, "3/3") {
		t.Fatalf("bad health state: checked=%v responsive=%v message=%q", checked, responsive, message)
	}
	if s.recordHealthResult(nil) {
		t.Fatal("successful check requested restart")
	}
	_, responsive, _, _, _ = s.XrayHealth()
	if !responsive {
		t.Fatal("successful check did not restore responsive state")
	}
}
