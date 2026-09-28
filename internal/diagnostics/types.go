package diagnostics

import "time"

type State string

const (
	StateHealthy     State = "healthy"
	StateDegraded    State = "degraded"
	StateUnavailable State = "unavailable"
	StateUnsafe      State = "unsafe"
)

type Repair struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Description  string `json:"description"`
	Target       string `json:"target,omitempty"`
	Confirmation string `json:"confirmation"`
}

type Check struct {
	ID       string  `json:"id"`
	Category string  `json:"category"`
	Name     string  `json:"name"`
	State    State   `json:"state"`
	Summary  string  `json:"summary"`
	Detail   string  `json:"detail,omitempty"`
	Repair   *Repair `json:"repair,omitempty"`
}

type Summary struct {
	Healthy     int `json:"healthy"`
	Degraded    int `json:"degraded"`
	Unavailable int `json:"unavailable"`
	Unsafe      int `json:"unsafe"`
}

type Report struct {
	GeneratedAt time.Time `json:"generatedAt"`
	Version     string    `json:"version"`
	Overall     State     `json:"overall"`
	Summary     Summary   `json:"summary"`
	Checks      []Check   `json:"checks"`
}

func NewReport(version string, generatedAt time.Time, checks []Check) Report {
	report := Report{
		GeneratedAt: generatedAt.UTC(),
		Version:     version,
		Overall:     StateHealthy,
		Checks:      append([]Check(nil), checks...),
	}
	for _, check := range checks {
		switch check.State {
		case StateUnsafe:
			report.Summary.Unsafe++
		case StateUnavailable:
			report.Summary.Unavailable++
		case StateDegraded:
			report.Summary.Degraded++
		default:
			report.Summary.Healthy++
		}
		if stateRank(check.State) > stateRank(report.Overall) {
			report.Overall = check.State
		}
	}
	return report
}

func (r Report) ExitCode() int {
	switch r.Overall {
	case StateUnsafe:
		return 2
	case StateUnavailable:
		return 1
	default:
		return 0
	}
}

func stateRank(state State) int {
	switch state {
	case StateDegraded:
		return 1
	case StateUnavailable:
		return 2
	case StateUnsafe:
		return 3
	default:
		return 0
	}
}
