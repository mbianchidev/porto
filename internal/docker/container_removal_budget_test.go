package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestContainerRemovalUsesOneLifecycleBudgetWithoutExtendingCallers(t *testing.T) {
	for _, action := range []string{"remove", "remove-volumes", "remove-force", "remove-force-volumes"} {
		for _, scenario := range []struct {
			name                     string
			configured, caller, want time.Duration
		}{
			{"lifecycle cleanup", defaultTimeout, 0, time.Minute},
			{"longer configured operation", 2 * time.Minute, 0, 2 * time.Minute},
			{"shorter caller", defaultTimeout, 2 * time.Second, 2 * time.Second},
		} {
			t.Run(action+"/"+scenario.name, func(t *testing.T) {
				var deadlines []time.Time
				runner := lifecycleDeadlineRunner(func(ctx context.Context, command runtimes.Command) ([]byte, error) {
					if strings.Join(command.Args, " ") == "container inspect synthetic-container" {
						return []byte(`[{"Id":"synthetic-original-id","Name":"/synthetic-container"}]`), nil
					}
					if len(command.Args) == 0 || command.Args[0] != "rm" {
						return nil, fmt.Errorf("unexpected removal command: %v", command.Args)
					}
					deadline, ok := ctx.Deadline()
					if !ok {
						return nil, errors.New("container removal has no bounded lifecycle deadline")
					}
					deadlines = append(deadlines, deadline)
					if remaining := time.Until(deadline); remaining <= scenario.want-time.Second || remaining > scenario.want {
						t.Errorf("container removal budget = %s, want %s with caller deadline preserved", remaining, scenario.want)
					}
					return nil, nil
				})
				manager := New(runner)
				manager.timeout = scenario.configured
				ctx := context.Background()
				if scenario.caller > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, scenario.caller)
					defer cancel()
				}
				if err := manager.ContainerAction(ctx, "synthetic-container", action); err != nil {
					t.Fatal(err)
				}
				expected := 1
				if strings.Contains(action, "force") {
					expected = 2
				}
				if len(deadlines) != expected {
					t.Fatalf("removal phases = %d, want %d", len(deadlines), expected)
				}
				if expected == 2 && !deadlines[0].Equal(deadlines[1]) {
					t.Fatal("force removal and stopped-container cleanup did not share one lifecycle deadline")
				}
			})
		}
	}
}

func TestContainerRemovalLifecycleBudgetPreservesCallerCancellation(t *testing.T) {
	for _, action := range []string{"remove", "remove-volumes", "remove-force", "remove-force-volumes"} {
		t.Run(action, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			runner := lifecycleDeadlineRunner(func(ctx context.Context, command runtimes.Command) ([]byte, error) {
				if strings.Join(command.Args, " ") == "container inspect synthetic-container" {
					return []byte(`[{"Id":"synthetic-original-id","Name":"/synthetic-container"}]`), nil
				}
				if len(command.Args) == 0 || command.Args[0] != "rm" {
					return nil, fmt.Errorf("unexpected removal command: %v", command.Args)
				}
				calls++
				cancel()
				return nil, ctx.Err()
			})
			if err := New(runner).ContainerAction(ctx, "synthetic-container", action); !errors.Is(err, context.Canceled) {
				t.Fatalf("caller cancellation was lost during removal: %v", err)
			}
			if calls != 1 {
				t.Fatalf("canceled removal submitted additional cleanup commands: %d", calls)
			}
		})
	}
}
