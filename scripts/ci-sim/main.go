// ci-sim estimates how long a workflow takes and what its critical path is, without running it.
//
// Two serial phases gated on each other cost the sum of their slowest members, so moving a `needs` or
// splitting a matrix moves the critical path. This shows that effect in a second instead of fifteen
// minutes.
//
// The job list comes from a real run's measured durations, and the ordering comes from the workflow
// files. Each measured job is attributed to the workflow job that produced it by longest-prefix match
// on its name: GitHub names a called workflow's jobs "<caller display name> / <callee display name>",
// and names a matrix leg from the caller's display template. A measured job that matches nothing, or a
// workflow job that no measured job matches, is reported rather than silently ignored.
//
// Refresh timings from any run of the same workflow:
//
//	gh api -X GET repos/<owner>/<repo>/actions/runs/<id>/jobs --paginate \
//	  --jq '[.jobs[] | {name: .name, seconds: ((.completed_at | fromdate) - (.started_at | fromdate))}]' \
//	  | tee scripts/ci-sim/timings.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// startupSeconds is the scheduling and checkout overhead a hosted job pays before its first real step.
// It is charged once per job, so a gated phase adds it once to the critical path.
const startupSeconds = 12

type workflow struct {
	Jobs map[string]job `yaml:"jobs"`
}

type job struct {
	Name  string    `yaml:"name"`
	Uses  string    `yaml:"uses"`
	Needs needsList `yaml:"needs"`
}

// needsList accepts both `needs: a` and `needs: [a, b]`.
type needsList []string

func (n *needsList) UnmarshalYAML(value *yaml.Node) error {
	var one string
	if err := value.Decode(&one); err == nil {
		*n = []string{one}
		return nil
	}
	var many []string
	if err := value.Decode(&many); err != nil {
		return err
	}
	*n = many
	return nil
}

type timing struct {
	Name    string `json:"name"`
	Seconds int    `json:"seconds"`
}

func main() {
	dir := flag.String("workflows", ".github/workflows", "directory holding the workflow files")
	entry := flag.String("entry", "main.yml", "workflow to schedule")
	timingsPath := flag.String("timings", "scripts/ci-sim/timings.json", "measured job durations")
	flag.Parse()

	wf, err := readWorkflow(filepath.Join(*dir, *entry))
	if err != nil {
		fail("read %s: %v", *entry, err)
	}
	timings, err := readTimings(*timingsPath)
	if err != nil {
		fail("read timings: %v", err)
	}

	// prefix each workflow job matches against, longest first so a specific name wins over a template
	type owner struct {
		id     string
		prefix string
		needs  []string
	}
	var owners []owner
	for id, j := range wf.Jobs {
		display := j.Name
		if display == "" {
			display = id
		}
		if i := strings.Index(display, "${{"); i >= 0 {
			display = strings.TrimSpace(display[:i])
		}
		owners = append(owners, owner{id: id, prefix: display, needs: j.Needs})
	}
	sort.Slice(owners, func(i, k int) bool { return len(owners[i].prefix) > len(owners[k].prefix) })

	ownerOf := map[string]string{}   // measured job name -> workflow job id
	jobsOf := map[string][]string{}  // workflow job id -> measured job names
	var orphans []string
	for _, t := range timings {
		matched := ""
		for _, o := range owners {
			if strings.HasPrefix(t.Name, o.prefix) {
				matched = o.id
				break
			}
		}
		if matched == "" {
			orphans = append(orphans, t.Name)
			continue
		}
		ownerOf[t.Name] = matched
		jobsOf[matched] = append(jobsOf[matched], t.Name)
	}

	needsOf := map[string][]string{}
	for _, o := range owners {
		needsOf[o.id] = o.needs
	}

	seconds := map[string]int{}
	for _, t := range timings {
		seconds[t.Name] = t.Seconds
	}

	// schedule: a job starts once every measured job of every job it needs has finished
	names := make([]string, 0, len(ownerOf))
	for name := range ownerOf {
		names = append(names, name)
	}
	sort.Strings(names)

	finish := map[string]int{}
	start := map[string]int{}
	blocker := map[string]string{}
	for pass := 0; pass <= len(names); pass++ {
		for _, name := range names {
			earliest, blockedBy := 0, ""
			for _, need := range needsOf[ownerOf[name]] {
				for _, upstream := range jobsOf[need] {
					if finish[upstream] > earliest {
						earliest, blockedBy = finish[upstream], upstream
					}
				}
			}
			start[name] = earliest
			blocker[name] = blockedBy
			finish[name] = earliest + startupSeconds + seconds[name]
		}
	}

	last := ""
	for _, name := range names {
		if last == "" || finish[name] > finish[last] {
			last = name
		}
	}

	fmt.Printf("estimated elapsed: %s (%d jobs, %ds startup charged per job)\n\n",
		mmss(finish[last]), len(names), startupSeconds)

	fmt.Println("critical path:")
	var chain []string
	for name := last; name != ""; name = blocker[name] {
		chain = append([]string{name}, chain...)
	}
	for _, name := range chain {
		fmt.Printf("  %6s -> %6s  %4ds  %s\n", mmss(start[name]), mmss(finish[name]), seconds[name], name)
	}

	fmt.Println("\nphases (jobs that start at the same time):")
	byStart := map[int][]string{}
	for _, name := range names {
		byStart[start[name]] = append(byStart[start[name]], name)
	}
	var starts []int
	for s := range byStart {
		starts = append(starts, s)
	}
	sort.Ints(starts)
	for _, s := range starts {
		group := byStart[s]
		slowest, total := "", 0
		for _, name := range group {
			if seconds[name] > total {
				slowest, total = name, seconds[name]
			}
		}
		fmt.Printf("  at %6s: %2d job(s), slowest %ds (%s)\n", mmss(s), len(group), total, slowest)
	}

	if len(orphans) > 0 {
		fmt.Printf("\n%d measured job(s) matched no workflow job and were left out:\n", len(orphans))
		for _, name := range orphans {
			fmt.Printf("  %s\n", name)
		}
	}
	for _, o := range owners {
		if len(jobsOf[o.id]) == 0 {
			fmt.Printf("\nworkflow job %q (%s) has no measured duration and was left out\n", o.id, o.prefix)
		}
	}
}

func readTimings(path string) ([]timing, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var list []timing
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func readWorkflow(path string) (*workflow, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var wf workflow
	if err := yaml.Unmarshal(body, &wf); err != nil {
		return nil, err
	}
	return &wf, nil
}

func mmss(seconds int) string {
	return fmt.Sprintf("%d:%02d", seconds/60, seconds%60)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ci-sim: "+format+"\n", args...)
	os.Exit(1)
}
