package cli

import (
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/itskills/kubedrill/internal/brand"
	"github.com/itskills/kubedrill/internal/daemon"
	"github.com/itskills/kubedrill/internal/scenario"
)

func (a *app) challengeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "challenge",
		Aliases: []string{"challenges", "ch"},
		Short:   "Troubleshooting challenges: list, start, status, check, reset, stop, validate",
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List available challenges",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var c daemon.CatalogResponse
			if err := a.call("GET", "/challenges", nil, &c); err != nil {
				return err
			}
			tw := tabwriter.NewWriter(a.out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tDIFFICULTY\tMIN\tSTATUS\tTITLE")
			for _, s := range c.Scenarios {
				status := ""
				if s.Solved {
					status = "solved"
				}
				if s.ID == c.Active {
					status = "ACTIVE"
				}
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", s.ID, s.Difficulty, s.EstimatedMinutes, status, s.Title)
			}
			_ = tw.Flush()
			for _, w := range c.Warnings {
				fmt.Fprintf(a.err, "warning: %s\n", w)
			}
			a.printf("\nStart one with `%s challenge start <id>`.\n", brand.Binary)
			return nil
		},
	}
	start := &cobra.Command{
		Use:   "start [id]",
		Short: "Start a challenge (injects a fault into the simulated cluster)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			si, err := a.server()
			if err != nil {
				return err
			}
			id := ""
			if len(args) == 1 {
				id = args[0]
			}
			var c daemon.ChallengeResponse
			if err := a.call("POST", "/challenge/start", daemon.StartRequest{ID: id}, &c); err != nil {
				return err
			}
			a.printf("Challenge started: %s  [%s]\n", c.Title, c.Difficulty)
			a.printf("ID: %s\n\n", c.ID)
			a.printf("%s\n", indent(strings.TrimSpace(c.Description), "  "))
			if c.Target.Mode == "existing" {
				a.printf("\nThe fault was injected into your existing %s %s/%s.\n", c.Target.Kind, c.Target.Namespace, c.Target.Name)
			}
			a.printf("\nUseful: kubectl --context %s get pods   |   %s hint   |   %s challenge check\n", brand.ContextName, brand.Binary, brand.Binary)
			a.contextNotice(si)
			return nil
		},
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Show the active challenge and the current state of its success criteria",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var c daemon.ChallengeResponse
			if err := a.call("GET", "/challenge", nil, &c); err != nil {
				return err
			}
			a.printf("Challenge: %s (%s)\n", c.Title, c.ID)
			a.printf("Target:    %s %s/%s (%s)\n", c.Target.Kind, c.Target.Namespace, c.Target.Name, c.Target.Mode)
			a.printf("Hints:     %d of %d revealed\n", c.HintsRevealed, c.HintsTotal)
			a.printf("Solution:  %s\n", map[bool]string{true: "revealed", false: "not revealed"}[c.SolutionRevealed])
			a.printf("Result:    %s\n\n", resultText(&c))
			a.printf("Task:\n%s\n\n", indent(strings.TrimSpace(c.Description), "  "))
			a.printCheck(c.Check)
			if c.Check != nil && c.Check.Passed && !c.Solved {
				a.printf("\nAll criteria pass. Run `%s challenge check` to complete the challenge.\n", brand.Binary)
			}
			return nil
		},
	}
	check := &cobra.Command{
		Use:   "check",
		Short: "Evaluate the end state of the cluster against the success criteria",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var c daemon.ChallengeResponse
			if err := a.call("POST", "/challenge/check", nil, &c); err != nil {
				return err
			}
			a.printCheck(c.Check)
			if c.Check.Passed {
				if c.SelfSolved {
					a.printf("\nSolved! Well done – you fixed it on your own (%d hint(s) used).\n", c.HintsRevealed)
				} else {
					a.printf("\nThe cluster is fixed. Because the solution was revealed, this counts as completed with help.\n")
				}
				a.printf("End the challenge with `%s challenge stop` (restores the state before the challenge) or `%s challenge stop --keep`.\n", brand.Binary, brand.Binary)
				return nil
			}
			a.printf("\nNot solved yet. Need help? `%s hint` reveals the next hint.\n", brand.Binary)
			return &exitError{code: 1}
		},
	}
	reset := &cobra.Command{
		Use:   "reset",
		Short: "Restore the broken starting point of the active challenge",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var c daemon.ChallengeResponse
			if err := a.call("POST", "/challenge/reset", nil, &c); err != nil {
				return err
			}
			a.printf("Challenge %q reset to its starting point (reset #%d). Hints already revealed stay revealed.\n", c.ID, c.Resets)
			return nil
		},
	}
	var keep bool
	stop := &cobra.Command{
		Use:   "stop",
		Short: "End the active challenge and restore the cluster state from before it started",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var c daemon.ChallengeResponse
			if err := a.call("POST", "/challenge/stop", daemon.StopRequest{Restore: !keep}, &c); err != nil {
				return err
			}
			a.printf("Challenge %q ended: %s.\n", c.ID, resultText(&c))
			if keep {
				a.printf("The cluster state was kept as it is.\n")
			} else {
				a.printf("The cluster was restored to the state before the challenge.\n")
			}
			return nil
		},
	}
	stop.Flags().BoolVar(&keep, "keep", false, "keep the current cluster state instead of restoring")
	validate := &cobra.Command{
		Use:   "validate <file-or-directory>...",
		Short: "Validate scenario YAML files (offline, no server needed)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			failed := 0
			total := 0
			for _, p := range args {
				files, err := scenario.FindFiles(p)
				if err != nil {
					return err
				}
				for _, f := range files {
					total++
					data, err := readFile(f)
					if err == nil {
						_, err = scenario.Parse(data, f)
					}
					if err != nil {
						failed++
						fmt.Fprintf(a.err, "%v\n", err)
						continue
					}
					a.printf("ok   %s\n", f)
				}
			}
			if total == 0 {
				return errors.New("no .yaml/.yml files found")
			}
			if failed > 0 {
				return fmt.Errorf("%d of %d scenario file(s) invalid", failed, total)
			}
			return nil
		},
	}
	cmd.AddCommand(list, start, status, check, reset, stop, validate)
	return cmd
}

func (a *app) hintCmd() *cobra.Command {
	var level int
	cmd := &cobra.Command{
		Use:   "hint",
		Short: "Reveal the next hint for the active challenge (or a specific --level)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var c daemon.ChallengeResponse
			if err := a.call("POST", "/challenge/hint", daemon.HintRequest{Level: level}, &c); err != nil {
				return err
			}
			a.printf("Hint %d/%d:\n%s\n", c.HintLevel, c.HintsTotal, indent(strings.TrimSpace(c.Hint), "  "))
			if c.HintLevel < c.HintsTotal {
				a.printf("\nStill stuck? `%s hint` shows hint %d.\n", brand.Binary, c.HintLevel+1)
			} else {
				a.printf("\nThat was the last hint. `%s solve` explains the full solution.\n", brand.Binary)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&level, "level", 0, "hint level to show (1 = gentle nudge, higher = more concrete)")
	return cmd
}

func (a *app) solveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "solve",
		Short: "Show diagnosis, root cause and fix of the active challenge (does not fix it for you)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var c daemon.ChallengeResponse
			if err := a.call("POST", "/challenge/solve", nil, &c); err != nil {
				return err
			}
			s := c.Solution
			a.printf("Solution for %q\n\n", c.Title)
			a.printf("Diagnosis:\n%s\n\n", indent(strings.TrimSpace(s.Diagnosis), "  "))
			a.printf("Root cause:\n%s\n\n", indent(strings.TrimSpace(s.RootCause), "  "))
			a.printf("Fix:\n%s\n", indent(strings.TrimSpace(s.Fix), "  "))
			if len(s.Commands) > 0 {
				a.printf("\nCommands (add --context %s):\n", brand.ContextName)
				for _, c := range s.Commands {
					a.printf("  %s\n", c)
				}
			}
			if c.Explanation != "" {
				a.printf("\nBackground:\n%s\n", indent(strings.TrimSpace(c.Explanation), "  "))
			}
			a.printf("\nThe cluster was NOT changed. Apply the fix yourself, then run `%s challenge check`.\n", brand.Binary)
			a.printf("(Revealing the solution is recorded: a later check counts as \"completed with help\".)\n")
			return nil
		},
	}
}

func (a *app) printCheck(r *scenario.CheckResult) {
	if r == nil {
		return
	}
	a.printf("Success criteria:\n")
	for _, c := range r.Criteria {
		mark := "[ ]"
		if c.Passed {
			mark = "[x]"
		}
		a.printf("  %s %s", mark, c.Description)
		if c.Detail != "" {
			a.printf("  (%s)", c.Detail)
		}
		a.printf("\n")
	}
}

func resultText(c *daemon.ChallengeResponse) string {
	switch {
	case c.Solved && c.SelfSolved:
		return "solved on your own"
	case c.Solved:
		return "completed with help (solution was revealed)"
	default:
		return "not solved yet"
	}
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n")
}
