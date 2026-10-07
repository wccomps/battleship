package apply

import (
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/wccomps/battleship/internal/config"
)

// battleship serve waits web.shutdown_timeout for jobs to stop, so the
// default must outlast StopBudget.
func TestDefaultShutdownOutlastsStop(t *testing.T) {
	if got := config.Default().Web.ShutdownTimeout; got <= StopBudget {
		t.Errorf("default web.shutdown_timeout = %s, want more than the %s stop budget", got, StopBudget)
	}
}

// The shipped Kubernetes config keeps StopBudget < web.shutdown_timeout <
// terminationGracePeriodSeconds, so rolling updates never kill a stopping
// replica.
func TestKubernetesShutdownChain(t *testing.T) {
	read := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	m := regexp.MustCompile(`terminationGracePeriodSeconds: (\d+)`).FindStringSubmatch(read("../../deploy/k8s/base/deployment.yaml"))
	if m == nil {
		t.Fatal("deployment.yaml sets no terminationGracePeriodSeconds")
	}
	secs, _ := strconv.Atoi(m[1])
	grace := time.Duration(secs) * time.Second
	for _, path := range []string{"../../deploy/k8s/base/battleship.toml", "../../deploy/k8s/overlays/example/battleship.toml"} {
		var file struct {
			Web struct {
				ShutdownTimeout string `toml:"shutdown_timeout"`
			} `toml:"web"`
		}
		if _, err := toml.DecodeFile(path, &file); err != nil {
			t.Fatal(err)
		}
		d, err := time.ParseDuration(file.Web.ShutdownTimeout)
		if err != nil {
			t.Fatalf("%s: web.shutdown_timeout: %v", path, err)
		}
		if d <= StopBudget || d >= grace {
			t.Errorf("%s: shutdown_timeout %s, want between the %s stop budget and the %s grace period", path, d, StopBudget, grace)
		}
	}
}
