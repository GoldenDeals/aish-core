package test_test

import (
	"os"
	"path/filepath"
	"testing"

	aish "github.com/GoldenDeals/aish-core"
	"github.com/GoldenDeals/aish-core/modules/policy/cedar"
)

// Stage 2 (§11): policy.cedar replaces policy.ask in the stage 1 scenario with
// no other change, and the Cedar policy decides what is asked.
func TestStage2CedarSwap(t *testing.T) {
	t.Parallel()

	for _, fe := range frontends {
		for _, tc := range []struct{ name, answer, want string }{
			{"approved", "yes", "world"},
			{"refused", "no", "hello"},
		} {
			t.Run(fe.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()

				files, store := t.TempDir(), t.TempDir()
				path := filepath.Join(files, "a.txt")
				if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
					t.Fatal(err)
				}
				rules := filepath.Join(t.TempDir(), "p.cedar")
				const policy = `permit(principal, action == Action::"fs.read", resource);
ask(principal, action == Action::"fs.write", resource);
forbid(principal, action, resource) when { resource.path like "*/.env*" };
`
				if err := os.WriteFile(rules, []byte(policy), 0o600); err != nil {
					t.Fatal(err)
				}
				llm := llmScript()
				inst, drv := fe.instance(t, tc.answer)
				s := sessionWith(t, aish.Instance{Name: "policy.cedar", Module: &cedar.Policy{Globs: []string{rules}}}, "s2", "", store, files, llm, inst)

				if err := drv.say(s, "fix the greeting"); err != nil {
					t.Fatal(err)
				}
				drv.reply(t)
				drv.done(t)
				s.Wait()

				if got := readFile(t, path); got != tc.want {
					t.Errorf("file = %q, want %q", got, tc.want)
				}
				if drv.asked() != 1 {
					t.Errorf("user was asked %d times, want once", drv.asked())
				}
			})
		}
	}
}
