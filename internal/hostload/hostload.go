// Package hostload reads the 1-minute load average. A mutation run on an
// oversubscribed host inflates every test's duration, so per-mutant timeouts
// fire for contention instead of for the mutation, and the "verdict" measures
// the host. changeling refuses to start on such a host (COULD-NOT-RUN).
package hostload

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// Load1 returns the 1-minute load average: /proc/loadavg on Linux, sysctl
// vm.loadavg on darwin. An unreadable value is an error, never 0.
func Load1() (float64, error) {
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		f := strings.Fields(string(b))
		if len(f) > 0 {
			return strconv.ParseFloat(f[0], 64)
		}
	}
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return 0, fmt.Errorf("load average unreadable: no /proc/loadavg and sysctl vm.loadavg failed: %w", err)
	}
	// "{ 12.16 21.93 13.65 }"
	f := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{}"))
	if len(f) == 0 {
		return 0, fmt.Errorf("load average unparseable: %q", out)
	}
	return strconv.ParseFloat(f[0], 64)
}

// Check returns an error when load1 exceeds the core count.
func Check(load func() (float64, error), cores int) error {
	if cores <= 0 {
		cores = runtime.NumCPU()
	}
	l, err := load()
	if err != nil {
		return err
	}
	if l > float64(cores) {
		return fmt.Errorf("host load1 %.2f exceeds its %d cores: timeouts here measure contention, not mutants", l, cores)
	}
	return nil
}
