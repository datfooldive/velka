package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"
)

type Project struct {
	Name string            `yaml:"name"`
	Path string            `yaml:"path"`
	Vars map[string]string `yaml:"vars"`
	Env  map[string]string `yaml:"env"`
	Deps []string          `yaml:"deps"`
	Cmds []string          `yaml:"cmds"`
}

type Config struct {
	Vars     map[string]string `yaml:"vars"`
	Env      map[string]string `yaml:"env"`
	Projects []Project         `yaml:"projects"`
}

type result struct {
	name      string
	err       error
	failedCmd string
	skipped   bool
	dur       time.Duration
}

var (
	colors   = []string{"36", "33", "32", "35", "34", "91", "96", "93", "92", "95"}
	useColor bool
	outMu    sync.Mutex
)

func main() {
	cfgPath := flag.String("c", "velka.yaml", "config file")
	only := flag.String("only", "", "comma-separated project names to run")
	flag.Parse()

	if fi, err := os.Stdout.Stat(); err == nil {
		useColor = fi.Mode()&os.ModeCharDevice != 0 && os.Getenv("NO_COLOR") == ""
	}

	projects, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "velka:", err)
		os.Exit(2)
	}
	if err := checkDeps(projects); err != nil {
		fmt.Fprintln(os.Stderr, "velka:", err)
		os.Exit(2)
	}
	if *only != "" {
		projects = filter(projects, strings.Split(*only, ","))
	}
	if len(projects) == 0 {
		fmt.Fprintln(os.Stderr, "velka: no projects to run")
		os.Exit(2)
	}

	width := 0
	for _, p := range projects {
		width = max(width, len(p.Name))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	results := make([]result, len(projects))
	index := map[string]int{}
	done := make([]chan struct{}, len(projects))
	for i, p := range projects {
		index[p.Name] = i
		done[i] = make(chan struct{})
	}
	var wg sync.WaitGroup
	for i, p := range projects {
		wg.Go(func() {
			defer close(done[i])
			prefix := fmt.Sprintf("%-*s | ", width, p.Name)
			if useColor {
				prefix = "\x1b[" + colors[i%len(colors)] + "m" + prefix + "\x1b[0m"
			}
			for _, d := range p.Deps {
				<-done[index[d]]
				if results[index[d]].err != nil {
					results[i] = result{name: p.Name, err: fmt.Errorf("dep %s failed", d), skipped: true}
					(&lineWriter{prefix: prefix}).println(paint("33", "SKIP: "+results[i].err.Error()))
					return
				}
			}
			results[i] = runProject(ctx, p, prefix)
		})
	}
	wg.Wait()

	fmt.Println()
	failed := false
	for _, r := range results {
		mark := paint("32", "OK  ")
		line := fmt.Sprintf("%s %-*s  %s", mark, width, r.name, r.dur.Round(time.Millisecond))
		if r.skipped {
			failed = true
			line = fmt.Sprintf("%s %-*s  %v", paint("33", "SKIP"), width, r.name, r.err)
		} else if r.err != nil {
			failed = true
			line = fmt.Sprintf("%s %-*s  %s  %v", paint("31", "FAIL"), width, r.name, r.dur.Round(time.Millisecond), r.err)
			if r.failedCmd != "" {
				line += fmt.Sprintf(" (%s)", r.failedCmd)
			}
		}
		fmt.Println(line)
	}
	if failed {
		os.Exit(1)
	}
}

func loadConfig(path string) ([]Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	base := filepath.Dir(path)
	home, _ := os.UserHomeDir()
	for i := range cfg.Projects {
		p := &cfg.Projects[i]
		vars := merge(cfg.Vars, p.Vars)
		p.Env = merge(cfg.Env, p.Env)
		render := func(s string) string {
			if err == nil {
				s, err = renderVars(s, vars)
			}
			return s
		}
		p.Path = render(p.Path)
		for k, v := range p.Env {
			p.Env[k] = render(v)
		}
		for j, c := range p.Cmds {
			p.Cmds[j] = render(c)
		}
		if err != nil {
			return nil, fmt.Errorf("project %d (%s): %w", i+1, p.Name, err)
		}
		if p.Path == "~" || strings.HasPrefix(p.Path, "~/") {
			p.Path = filepath.Join(home, p.Path[1:])
		} else if !filepath.IsAbs(p.Path) {
			p.Path = filepath.Join(base, p.Path)
		}
		if p.Name == "" {
			p.Name = filepath.Base(p.Path)
		}
	}
	return cfg.Projects, nil
}

func merge(global, project map[string]string) map[string]string {
	m := maps.Clone(global)
	if m == nil {
		m = map[string]string{}
	}
	maps.Copy(m, project)
	return m
}

func renderVars(s string, vars map[string]string) (string, error) {
	if !strings.Contains(s, "${{") {
		return s, nil
	}
	t, err := template.New("").Delims("${{", "}}").Option("missingkey=error").Parse(s)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, vars); err != nil {
		return "", err
	}
	return b.String(), nil
}

func checkDeps(projects []Project) error {
	byName := map[string]Project{}
	for _, p := range projects {
		if _, ok := byName[p.Name]; ok {
			return fmt.Errorf("duplicate project name: %s", p.Name)
		}
		byName[p.Name] = p
	}
	state := map[string]int{}
	var visit func(name string, path []string) error
	visit = func(name string, path []string) error {
		switch state[name] {
		case 1:
			return fmt.Errorf("dependency cycle: %s -> %s", strings.Join(path, " -> "), name)
		case 2:
			return nil
		}
		state[name] = 1
		for _, d := range byName[name].Deps {
			if _, ok := byName[d]; !ok {
				return fmt.Errorf("project %s: unknown dep %s", name, d)
			}
			if err := visit(d, append(path, name)); err != nil {
				return err
			}
		}
		state[name] = 2
		return nil
	}
	for _, p := range projects {
		if err := visit(p.Name, nil); err != nil {
			return err
		}
	}
	return nil
}

func filter(projects []Project, names []string) []Project {
	byName := map[string]Project{}
	for _, p := range projects {
		byName[p.Name] = p
	}
	want := map[string]bool{}
	var add func(name string)
	add = func(name string) {
		p, ok := byName[name]
		if !ok || want[name] {
			return
		}
		want[name] = true
		for _, d := range p.Deps {
			add(d)
		}
	}
	for _, n := range names {
		add(strings.TrimSpace(n))
	}
	var out []Project
	for _, p := range projects {
		if want[p.Name] {
			out = append(out, p)
		}
	}
	return out
}

func runProject(ctx context.Context, p Project, prefix string) result {
	start := time.Now()
	r := result{name: p.Name}

	w := &lineWriter{prefix: prefix}
	if fi, err := os.Stat(p.Path); err != nil || !fi.IsDir() {
		r.err = fmt.Errorf("path not found: %s", p.Path)
		w.println(paint("31", r.err.Error()))
		r.dur = time.Since(start)
		return r
	}
	if len(p.Cmds) == 0 {
		r.err = fmt.Errorf("no cmds")
		r.dur = time.Since(start)
		return r
	}

	for _, c := range p.Cmds {
		w.println(paint("1", "$ "+c))
		cmd := shellCommand(ctx, c)
		cmd.Dir = p.Path
		cmd.Env = os.Environ()
		for k, v := range p.Env {
			cmd.Env = append(cmd.Env, k+"="+os.ExpandEnv(v))
		}
		cmd.Stdout = w
		cmd.Stderr = w
		cmd.WaitDelay = 2 * time.Second
		err := cmd.Run()
		w.flush()
		if err != nil {
			if ctx.Err() != nil {
				err = fmt.Errorf("interrupted")
			}
			r.err, r.failedCmd = err, c
			w.println(paint("31", "FAIL: "+err.Error()))
			break
		}
	}
	r.dur = time.Since(start)
	return r
}

type lineWriter struct {
	prefix string
	buf    []byte
}

func (w *lineWriter) Write(b []byte) (int, error) {
	w.buf = append(w.buf, b...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.println(string(bytes.TrimRight(w.buf[:i], "\r")))
		w.buf = w.buf[i+1:]
	}
	return len(b), nil
}

func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.println(string(w.buf))
		w.buf = w.buf[:0]
	}
}

func (w *lineWriter) println(s string) {
	outMu.Lock()
	fmt.Println(w.prefix + s)
	outMu.Unlock()
}

func paint(code, s string) string {
	if !useColor {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}
