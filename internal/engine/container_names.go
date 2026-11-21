package engine

// Owner: workstream B. Container IDs, names and display helpers.

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"time"
)

var containerNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// validContainerName reports whether name is accepted for a container
// (after stripping any single leading slash).
func validContainerName(name string) bool {
	return name != "" && containerNameRe.MatchString(name)
}

// Original word lists for generated container names.
var nameAdjectives = []string{
	"amber", "brisk", "calm", "cedar", "clear", "coral", "crisp", "dusk",
	"early", "fair", "fern", "flint", "frost", "gentle", "gleam", "golden",
	"granite", "harbor", "hazel", "ivory", "jade", "kind", "lively", "lunar",
	"maple", "meadow", "misty", "nimble", "north", "ochre", "olive", "opal",
	"pine", "placid", "quartz", "quiet", "rapid", "river", "rustic", "saffron",
	"silver", "slate", "solar", "steady", "stone", "swift", "timber", "vivid",
}

var nameNouns = []string{
	"alder", "anvil", "arch", "aspen", "badger", "beacon", "birch", "brook",
	"cairn", "canyon", "cliff", "clover", "comet", "crane", "creek", "crest",
	"delta", "dune", "falcon", "field", "finch", "fjord", "forge", "glade",
	"grove", "hawk", "heath", "heron", "inlet", "island", "kestrel", "larch",
	"linden", "lynx", "marsh", "otter", "peak", "pebble", "plover", "quail",
	"ridge", "robin", "shoal", "spruce", "summit", "terrace", "vale", "wren",
}

// generateContainerName picks an unused adjective_noun container name.
func generateContainerName(used func(string) bool) string {
	for range 64 {
		adj := nameAdjectives[randInt(len(nameAdjectives))]
		noun := nameNouns[randInt(len(nameNouns))]
		candidate := adj + "_" + noun
		if !used(candidate) {
			return candidate
		}
	}
	for {
		adj := nameAdjectives[randInt(len(nameAdjectives))]
		noun := nameNouns[randInt(len(nameNouns))]
		candidate := adj + "_" + noun + "_" + strconv.Itoa(randInt(10000))
		if !used(candidate) {
			return candidate
		}
	}
}

func randInt(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

// formatInspectTime formats a timestamp as docker inspect does, using
// 0001-01-01T00:00:00Z for zero times.
func formatInspectTime(t time.Time) string {
	if t.IsZero() {
		return "0001-01-01T00:00:00Z"
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// humanDuration formats a duration in docker ps style ("3 seconds", "2 minutes").
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Second:
		return "Less than a second"
	case d < 2*time.Second:
		return "1 second"
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	case d < 2*time.Minute:
		return "About a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 2*time.Hour:
		return "About an hour"
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%d weeks", int(d.Hours()/(24*7)))
	case d < 2*365*24*time.Hour:
		return fmt.Sprintf("%d months", int(d.Hours()/(24*30)))
	default:
		return fmt.Sprintf("%d years", int(d.Hours()/(24*365)))
	}
}

// containerStatusText formats a container's Status column as docker ps does.
func containerStatusText(st ContainerState, now time.Time) string {
	switch st.Status {
	case StatusCreated:
		return "Created"
	case StatusRunning:
		if st.StartedAt.IsZero() {
			return "Up"
		}
		return "Up " + humanDuration(now.Sub(st.StartedAt))
	case StatusRestarting:
		if st.FinishedAt.IsZero() {
			return fmt.Sprintf("Restarting (%d)", st.ExitCode)
		}
		return fmt.Sprintf("Restarting (%d) %s ago", st.ExitCode, humanDuration(now.Sub(st.FinishedAt)))
	case StatusRemoving:
		return "Removal In Progress"
	case StatusExited:
		if st.FinishedAt.IsZero() {
			return fmt.Sprintf("Exited (%d)", st.ExitCode)
		}
		return fmt.Sprintf("Exited (%d) %s ago", st.ExitCode, humanDuration(now.Sub(st.FinishedAt)))
	case StatusDead:
		return "Dead"
	default:
		return st.Status
	}
}
