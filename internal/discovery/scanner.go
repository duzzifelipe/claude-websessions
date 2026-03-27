package discovery

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type ProcessInfo struct {
	PID               int
	Binary            string
	Provider          string
	WorkDir           string
	Args              []string
	ExternalSessionID string
	ClaudeID          string
	StartTime         time.Time
}

func ProviderForBinary(path string) (string, bool) {
	switch filepath.Base(path) {
	case "claude":
		return "claude", true
	case "opencode":
		return "opencode", true
	default:
		return "", false
	}
}

func IsSupportedBinary(path string) bool {
	_, ok := ProviderForBinary(path)
	return ok
}

func IsClaudeBinary(path string) bool {
	provider, ok := ProviderForBinary(path)
	return ok && provider == "claude"
}

func ParseCmdline(cmdline string) (*ProcessInfo, error) {
	parts := strings.Fields(cmdline)
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty cmdline")
	}
	provider, ok := ProviderForBinary(parts[0])
	if !ok {
		return nil, fmt.Errorf("unsupported process: %s", parts[0])
	}

	info := &ProcessInfo{Binary: parts[0], Provider: provider, Args: parts[1:]}
	args := parts[1:]
	for i := range args {
		switch provider {
		case "opencode":
			if value, ok := argOrValue(args, i, "--session"); ok {
				info.ExternalSessionID = value
				continue
			}
			if value, ok := argOrValue(args, i, "--session-id"); ok {
				info.ExternalSessionID = value
				continue
			}
			if value, ok := argOrValue(args, i, "--resume"); ok && info.ExternalSessionID == "" {
				info.ExternalSessionID = value
			}
		default:
			if value, ok := argOrValue(args, i, "--resume"); ok {
				info.ExternalSessionID = value
				continue
			}
			if value, ok := argOrValue(args, i, "--session-id"); ok && info.ExternalSessionID == "" {
				info.ExternalSessionID = value
				continue
			}
			if value, ok := argOrValue(args, i, "--session"); ok && info.ExternalSessionID == "" {
				info.ExternalSessionID = value
			}
		}
	}
	if provider == "claude" {
		info.ClaudeID = info.ExternalSessionID
	}
	return info, nil
}

func argOrValue(args []string, i int, flag string) (string, bool) {
	if i >= len(args) {
		return "", false
	}
	arg := args[i]
	if arg == flag {
		if i+1 >= len(args) {
			return "", false
		}
		return args[i+1], true
	}
	prefix := flag + "="
	if strings.HasPrefix(arg, prefix) {
		return strings.TrimPrefix(arg, prefix), true
	}
	return "", false
}

// ResolveClaudeSessionID finds the active Claude session ID for a working directory
// by looking at .jsonl files in ~/.claude/projects/<project>/
// If processStartTime is provided, picks the file whose modification time is closest
// to (but after) the process start time, to avoid picking a different session's file
// when multiple Claude instances share the same working directory.
func ResolveClaudeSessionID(workDir string) string {
	return resolveSessionID(workDir, time.Time{})
}

// ResolveClaudeSessionIDForProcess resolves the session ID using the process start time
// to disambiguate when multiple sessions share the same working directory.
func ResolveClaudeSessionIDForProcess(workDir string, processStartTime time.Time) string {
	return resolveSessionID(workDir, processStartTime)
}

func resolveSessionID(workDir string, processStartTime time.Time) string {
	if workDir == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	projectName := strings.ReplaceAll(workDir, "/", "-")
	projectName = strings.ReplaceAll(projectName, ".", "-")
	projectDir := filepath.Join(home, ".claude", "projects", projectName)

	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return ""
	}

	type candidate struct {
		id      string
		modTime time.Time
	}
	var candidates []candidate
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		candidates = append(candidates, candidate{
			id:      strings.TrimSuffix(entry.Name(), ".jsonl"),
			modTime: info.ModTime(),
		})
	}
	if len(candidates) == 0 {
		return ""
	}

	// If no process start time, just pick the most recently modified
	if processStartTime.IsZero() {
		var bestID string
		var bestTime time.Time
		for _, c := range candidates {
			if c.modTime.After(bestTime) {
				bestTime = c.modTime
				bestID = c.id
			}
		}
		return bestID
	}

	// With process start time: pick the file that was active when the process started.
	// Find files modified after the process started (active sessions), then pick the one
	// whose mod time is closest to the start time (the one that started being written
	// around the same time the process launched).
	var bestID string
	var bestDelta time.Duration = 1<<63 - 1 // max duration
	for _, c := range candidates {
		if c.modTime.Before(processStartTime) {
			continue // file hasn't been written since process started
		}
		delta := c.modTime.Sub(processStartTime)
		if delta < bestDelta {
			bestDelta = delta
			bestID = c.id
		}
	}
	// If no files found after process start, fallback to most recent
	if bestID == "" {
		var bestTime time.Time
		for _, c := range candidates {
			if c.modTime.After(bestTime) {
				bestTime = c.modTime
				bestID = c.id
			}
		}
	}
	return bestID
}

func Scan() ([]ProcessInfo, error) {
	switch runtime.GOOS {
	case "linux":
		return scanLinux()
	case "darwin":
		return scanDarwin()
	default:
		return nil, fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

func scanLinux() ([]ProcessInfo, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("reading /proc: %w", err)
	}
	var results []ProcessInfo
	bootTime := linuxBootTime()
	ticksPerSecond := linuxClockTicksPerSecond()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdlineBytes, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		cmdline := strings.ReplaceAll(string(cmdlineBytes), "\x00", " ")
		cmdline = strings.TrimSpace(cmdline)
		info, err := ParseCmdline(cmdline)
		if err != nil {
			continue
		}
		info.PID = pid
		if startTime, err := linuxProcessStartTime(entry.Name(), bootTime, ticksPerSecond); err == nil {
			info.StartTime = startTime
		}
		cwd, err := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd"))
		if err == nil {
			info.WorkDir = cwd
		}
		// Resolve Claude session ID from project files if not in args.
		if info.Provider == "claude" && info.ExternalSessionID == "" && info.WorkDir != "" {
			info.ExternalSessionID = ResolveClaudeSessionIDForProcess(info.WorkDir, info.StartTime)
			info.ClaudeID = info.ExternalSessionID
		}
		results = append(results, *info)
	}
	return results, nil
}

func scanDarwin() ([]ProcessInfo, error) {
	// Use stable two-field format to find candidate PIDs
	out, err := exec.Command("ps", "-eo", "pid,comm").Output()
	if err != nil {
		return nil, fmt.Errorf("running ps: %w", err)
	}
	var results []ProcessInfo
	lines := strings.Split(string(out), "\n")
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if !IsSupportedBinary(fields[1]) {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}

		// Get full command line for this PID
		cmdOut, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			continue
		}
		cmdline := strings.TrimSpace(string(cmdOut))
		info, err := ParseCmdline(cmdline)
		if err != nil {
			continue
		}
		info.PID = pid
		if startTime, err := darwinProcessStartTime(pid); err == nil {
			info.StartTime = startTime
		}

		// Get working directory via lsof (standard on macOS)
		info.WorkDir = darwinCwd(pid)

		if info.Provider == "claude" && info.ExternalSessionID == "" && info.WorkDir != "" {
			info.ExternalSessionID = ResolveClaudeSessionIDForProcess(info.WorkDir, info.StartTime)
			info.ClaudeID = info.ExternalSessionID
		}
		results = append(results, *info)
	}
	return results, nil
}

func linuxBootTime() time.Time {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "btime ") {
			continue
		}
		secs, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 10, 64)
		if err != nil {
			return time.Time{}
		}
		return time.Unix(secs, 0)
	}
	return time.Time{}
}

func linuxClockTicksPerSecond() int64 {
	out, err := exec.Command("getconf", "CLK_TCK").Output()
	if err != nil {
		return 100
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || value <= 0 {
		return 100
	}
	return value
}

func linuxProcessStartTime(pid string, bootTime time.Time, ticksPerSecond int64) (time.Time, error) {
	if bootTime.IsZero() || ticksPerSecond <= 0 {
		return time.Time{}, fmt.Errorf("missing boot time or ticks")
	}
	statBytes, err := os.ReadFile(filepath.Join("/proc", pid, "stat"))
	if err != nil {
		return time.Time{}, err
	}
	ticks, err := parseLinuxProcStatStartTicks(strings.TrimSpace(string(statBytes)))
	if err != nil {
		return time.Time{}, err
	}
	start := bootTime.Add(time.Duration(float64(time.Second) * float64(ticks) / float64(ticksPerSecond)))
	return start, nil
}

func parseLinuxProcStatStartTicks(stat string) (int64, error) {
	end := strings.LastIndex(stat, ")")
	if end == -1 || end+2 >= len(stat) {
		return 0, fmt.Errorf("invalid /proc stat format")
	}
	fields := strings.Fields(stat[end+2:])
	if len(fields) <= 19 {
		return 0, fmt.Errorf("missing starttime field")
	}
	return strconv.ParseInt(fields[19], 10, 64)
}

func darwinProcessStartTime(pid int) (time.Time, error) {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return time.Time{}, err
	}
	return parseDarwinLStart(strings.TrimSpace(string(out)))
}

func parseDarwinLStart(value string) (time.Time, error) {
	return time.ParseInLocation("Mon Jan _2 15:04:05 2006", value, time.Local)
}

// darwinCwd returns the current working directory of a process on macOS using lsof.
func darwinCwd(pid int) string {
	out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") && len(line) > 1 {
			return line[1:]
		}
	}
	return ""
}
