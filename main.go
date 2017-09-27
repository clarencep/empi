package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// EmpiConfig is the struct of this program's config
type EmpiConfig struct {
	Version   string         `json:"version"`
	Processes []*EmpiProcess `json:"processes"`
}

// EmpiProcess is the struct of each process' config
type EmpiProcess struct {
	Name                string   `json:"name"`
	Program             string   `json:"program"`
	Arguments           []string `json:"arguments"`
	WorkDir             string   `json:"work_dir"`
	Stdin               string   `json:"stdin"`
	Stdout              string   `json:"stdout"`
	Stderr              string   `json:"stderr"`
	AppendStdout        bool     `json:"append_stdout"`
	AppendStderr        bool     `json:"append_stderr"`
	User                string   `json:"user"`
	Group               string   `json:"group"`
	Umask               string   `json:"umask"`
	AutoRestart         string   `json:"auto_restart"`
	StartRetries        int32    `json:"start_retries"`
	RestartIntervalSecs float64  `json:"restart_interval_secs"`
	MinRunSecs          float64  `json:"min_run_secs"`
	ExitCodes           []int    `json:"exit_codes"`
	StopSignal          string   `json:"stop_signal"`
	StopWaitSecs        float64  `json:"stop_wait_secs"`
	Env                 []string `json:"env"`
	Disabled            bool     `json:"disabled"`

	lastStartTime      time.Time
	lastError          error
	unexpectedStopsNum int32
	isStopped          int32
	cmd                *exec.Cmd
	stopSignalValue    os.Signal
	dieSignalChanList  []chan int

	// uid, gid 都可能等于0，无效值用-1来存储
	uid int64
	gid int64
}

// DefaultConfigFilePath is the default path of configuration file
var DefaultConfigFilePath = "/etc/empi-config.json"

// SIGUSR1 is posix SIGUSR1
const SIGUSR1 = syscall.Signal(10)

// SIGUSR2 is posix SIGUSR2
const SIGUSR2 = syscall.Signal(12)

const defaultFileOpenPerm = 0755

func main() {
	var err error
	var configFilePath string
	var testConfigOnly bool

	flag.StringVar(&configFilePath, "c", DefaultConfigFilePath, "Specify the path of configuration file")
	flag.BoolVar(&testConfigOnly, "t", false, "If you just wanna test the configuration")
	flag.Parse()

	log.Println("parsing config file:", configFilePath)

	configStr, err := ioutil.ReadFile(configFilePath)
	dieIfError(err)

	configStr = []byte(stripJSONComments(string(configStr)))

	var config EmpiConfig
	err = json.Unmarshal(configStr, &config)
	dieIfError(err)

	config.checkAndFullFill()

	log.Printf("parsed config: %#v", config)
	config.dump()

	if testConfigOnly {
		return
	}

	log.Println("begin run...")

	config.runAndWait()
}

func (config *EmpiConfig) checkAndFullFill() {
	for _, proc := range config.Processes {
		if !proc.Disabled {
			proc.checkAndFullFill()
		}
	}
}

func (config *EmpiConfig) run() {
	for _, proc := range config.Processes {
		if !proc.Disabled {
			go proc.runForever()
		}
	}
}

func (config *EmpiConfig) runAndWait() {
	// handle signals
	signals := make(chan os.Signal, 1)

	// setup signals
	// KILL signal cannot be caught, so just wait these signals:
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGABRT, syscall.SIGQUIT, syscall.SIGHUP)

	// run the process!
	config.run()

	s := <-signals
	log.Printf("Got signal: %v", s)

	stopWaitGroup := sync.WaitGroup{}

	for _, proc := range config.Processes {
		proc := proc
		if !proc.Disabled {
			stopWaitGroup.Add(1)
			go func() {
				defer stopWaitGroup.Done()
				proc.stop()
			}()
		}
	}

	stopWaitGroup.Wait()

	log.Printf("EMPI stopped")
	os.Exit(0)
}

func (config *EmpiConfig) dumpAsString() string {
	output := make([]string, 5+len(config.Processes))[:0]
	output = append(output, fmt.Sprintf("EmpiConfig{"))
	output = append(output, fmt.Sprintf("  Version=%s", config.Version))
	output = append(output, fmt.Sprintf("  Processes=["))

	for i, proc := range config.Processes {
		output = append(output, fmt.Sprintf("     [%d]=%#v", i, proc))
	}

	output = append(output, "    ]")
	output = append(output, "}")
	return strings.Join(output, "\n")
}

func (config *EmpiConfig) dump() {
	log.Print(config.dumpAsString())
}

func (proc *EmpiProcess) checkAndFullFill() {
	var err error

	if proc.Program == "" {
		log.Fatalf("ConfigError: `program` is required.")
		return
	}

	if proc.Name == "" {
		_, proc.Name = path.Split(proc.Program)
	}

	if proc.AutoRestart == "" {
		proc.AutoRestart = "unexpected"
	} else if proc.AutoRestart == "yes" {
		proc.AutoRestart = "always"
	}

	if arraySearchStr(proc.AutoRestart, []string{"unexpected", "always", "no", "never"}) < 0 {
		log.Fatalf("ConfigError: `auto_restart` must be one of `unexpected`, `yes`, `always`, `no` or `never`")
		return
	}

	if proc.StopSignal == "" {
		proc.StopSignal = "SIGTERM"
	}

	proc.stopSignalValue, err = parseSignal(proc.StopSignal)
	if err != nil {
		log.Fatalf("ConfigError: `stop_signal` is not valid! it must be one of SIGINT/SIGKILL/SIGTERM/SIGABRT/SIGQUIT/SIGHUP/SIGUSR1/SIGUSR2.")
		return
	}

	if proc.User != "" {
		uid, err := parseUid(proc.User)
		if err != nil {
			log.Fatalf("ConfigError: failed to parse uid: %v", err)
			return
		}

		proc.uid = int64(uid)
	} else {
		proc.uid = -1
	}

	if proc.Group != "" {
		gid, err := parseGid(proc.Group)
		if err != nil {
			log.Fatalf("ConfigError: failed to parse gid: %v", err)
			return
		}

		proc.gid = int64(gid)
	} else {
		proc.gid = -1
	}
}

func (proc *EmpiProcess) runForever() {
	for !proc.shouldDie() {
		err := proc.tryRunOnce()
		proc.logf("tryRunOnce error: %v  (err != nil: %v)", err, err != nil)
		proc.logf("shouldDie: %v", proc.shouldDie())

		if proc.AutoRestart == "always" || (proc.AutoRestart == "unexpected" && err != nil) {
			time.Sleep(time.Duration(proc.RestartIntervalSecs * float64(time.Second)))
			continue
		}

		break
	}

	proc.logf("stopped.")
	proc.logf("detail: %#v", proc)
}

func (proc *EmpiProcess) tryRunOnce() (runError error) {
	defer func() {
		if r := recover(); r != nil {
			err, ok := r.(error)
			if !ok {
				err = fmt.Errorf("RecoverError: %v", r)
			}

			log.Printf("Got error when try run %s: %v", proc.Name, err)
			proc.recordError(err)
			runError = err
		}
	}()

	proc.lastStartTime = time.Now()

	exitCode, err := proc.exec()
	proc.logf("exit with error code: %d, error: %#v", exitCode, err)
	proc.notifyDieSignals(exitCode)

	if err != nil {
		proc.recordError(err)
		return err
	}

	if proc.isUnexpectedExitCode(exitCode) {
		runError = fmt.Errorf("Unexpected exit code %d", exitCode)
		proc.recordError(runError)
		return
	}

	if proc.MinRunSecs > 0 {
		procRunTimeSecs := float64(time.Now().Sub(proc.lastStartTime)) / float64(time.Second)
		if procRunTimeSecs < float64(proc.MinRunSecs) {
			runError = fmt.Errorf("Proc exit to quickly after %.3fs", procRunTimeSecs)
			proc.recordError(runError)
			return
		}
	}

	return nil
}

func (proc *EmpiProcess) exec() (int, error) {
	var exeFile string
	var err error

	programDir, _ := path.Split(proc.Program)
	if programDir == "" {
		exeFile, err = exec.LookPath(proc.Program)
		if err != nil {
			exeFile = proc.Program
		}
	} else {
		exeFile = proc.Program
	}

	proc.logf("executing %s %#v", exeFile, proc.Arguments)

	cmd := exec.Command(exeFile, proc.Arguments...)

	// specify work_dir
	if proc.WorkDir != "" {
		cmd.Dir = proc.WorkDir
	}

	// parse stdin
	stdinFile := proc.Stdin
	if stdinFile == "" || stdinFile == "-" || stdinFile == "inherit" {
		cmd.Stdin = os.Stdin
	} else {
		if stdinFile == "null" {
			stdinFile = os.DevNull
		}

		stdinFileHandle, err := os.Open(stdinFile)
		if err != nil {
			proc.logf("Failed to open stdin(%s), error: %#v", stdinFile, err)
			return -1, err
		}

		cmd.Stdin = stdinFileHandle
		defer stdinFileHandle.Close()
	}

	// parse stdout:
	stdoutFile := proc.Stdout
	if stdoutFile == "" || stdoutFile == "-" || stdoutFile == "inherit" {
		cmd.Stdout = os.Stdout
	} else if stdoutFile == "&stderr" {
		// 这种情况下需要再后面再来赋值
	} else {
		if stdoutFile == "null" {
			stdoutFile = os.DevNull
		}

		stdoutFileFlag := os.O_CREATE | os.O_RDWR
		if proc.AppendStdout {
			stdoutFileFlag |= os.O_APPEND
		}

		stdoutFileHandle, err := os.OpenFile(stdoutFile, stdoutFileFlag, defaultFileOpenPerm)
		if err != nil {
			proc.logf("Failed to open stdout(%s), error: %#v", stdoutFile, err)
			return -1, err
		}

		cmd.Stdout = stdoutFileHandle
		defer stdoutFileHandle.Close()
	}

	stderrFile := proc.Stderr
	if stderrFile == "" || stderrFile == "-" || stderrFile == "inherit" {
		cmd.Stderr = os.Stderr
	} else if stderrFile == "&stdout" {
		if stdoutFile == "&stderr" {
			err = errors.New("Circular reference: stdout=&stderr, stderr=&stdout")
			proc.logf("Error: %s", err.Error())
			return -1, err
		}

		cmd.Stderr = cmd.Stdout
	} else {
		if stderrFile == "null" {
			stderrFile = os.DevNull
		}

		stderrFileFlag := os.O_CREATE | os.O_RDWR
		if proc.AppendStderr {
			stderrFileFlag |= os.O_APPEND
		}

		stderrFileHandle, err := os.OpenFile(stderrFile, stderrFileFlag, defaultFileOpenPerm)
		if err != nil {
			proc.logf("Failed to open stderr(%s), error: %#v", stderrFile, err)
			return -1, err
		}

		cmd.Stderr = stderrFileHandle
		defer stderrFileHandle.Close()
	}

	if stdoutFile == "&stderr" {
		cmd.Stdout = cmd.Stderr
	}

	// set environment variables:
	if len(proc.Env) > 0 {
		cmd.Env = append(os.Environ(), proc.Env...)
	}

	// set user and group
	if proc.uid >= 0 || proc.gid >= 0 {
		err := setCmdCredential(cmd, proc.uid, proc.gid)
		if err != nil {
			proc.logf("Error: %v", err)
			return -1, err
		}
	}

	// todo:  umask

	err = cmd.Start()
	if err != nil {
		return -1, err
	}

	proc.cmd = cmd

	err = cmd.Wait()
	if err != nil {
		proc.logf("wait result: %#v", err)
		if exitErr, ok := err.(*exec.ExitError); ok {
			// This program has exited with exit code != 0
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				retCode := status.ExitStatus()
				proc.logf("wait status: %d", retCode)
				return retCode, nil
			}

			return -1, exitErr
		}

		return -1, err
	}

	return 0, nil
}

func (proc *EmpiProcess) recordError(err error) {
	proc.lastError = err
	proc.unexpectedStopsNum++
}

func (proc *EmpiProcess) shouldDie() bool {
	return atomic.LoadInt32(&proc.isStopped) != 0 || (proc.StartRetries > 0 && proc.unexpectedStopsNum >= proc.StartRetries)
}

func (proc *EmpiProcess) kill() {
	atomic.StoreInt32(&proc.isStopped, 1)
	if proc.cmd != nil && proc.cmd.Process != nil {
		proc.cmd.Process.Kill()
	}
}

func (proc *EmpiProcess) stop() bool {
	proc.logf("stopping...")
	atomic.StoreInt32(&proc.isStopped, 1)
	if proc.cmd != nil && proc.cmd.Process != nil {
		// stop_signal and stop_wait_secs
		process := proc.cmd.Process
		process.Signal(proc.stopSignalValue)
		if proc.StopWaitSecs > 0 {
			return proc.waitDieOrKill(proc.StopWaitSecs)
		}
	}

	return true
}

func (proc *EmpiProcess) waitDieOrKill(waitSecs float64) bool {
	waitTimer := time.After(time.Duration(waitSecs * float64(time.Second)))
	procDieSignal := make(chan int, 1)
	proc.registerDieSignalChan(procDieSignal)
	defer proc.unregisterDieSignalChan(procDieSignal)

	for {
		select {
		case <-waitTimer:
			proc.kill()
			return false
		case <-procDieSignal:
			return true
		case <-time.After(500 * time.Millisecond):
			if !proc.isRunning() {
				return true
			}
		}

		runtime.Gosched()
	}
}

func (proc *EmpiProcess) registerDieSignalChan(ch chan int) {
	proc.dieSignalChanList = append(proc.dieSignalChanList, ch)
}

func (proc *EmpiProcess) unregisterDieSignalChan(ch chan int) {
	if len(proc.dieSignalChanList) <= 0 {
		return
	}

	list := make([]chan int, len(proc.dieSignalChanList))
	j := 0
	for _, x := range proc.dieSignalChanList {
		if x != ch {
			list[j] = x
			j++
		}
	}

	proc.dieSignalChanList = list
}

func (proc *EmpiProcess) notifyDieSignals(retCode int) {
	for _, ch := range proc.dieSignalChanList {
		ch <- retCode
	}
}

func (proc *EmpiProcess) isRunning() bool {
	if atomic.LoadInt32(&proc.isStopped) != 0 {
		return true
	}

	if proc.cmd == nil {
		return false
	}

	if proc.cmd.Process == nil {
		return false
	}

	err := proc.cmd.Process.Signal(syscall.Signal(0))
	if err != nil {
		return false
	}

	return true
}

func (proc *EmpiProcess) isUnexpectedExitCode(retCode int) bool {
	if retCode < 0 {
		return true
	}

	for _, x := range proc.ExitCodes {
		if retCode == x {
			return true
		}
	}

	return false
}

func (proc *EmpiProcess) logf(fmt string, args ...interface{}) {
	log.Printf("[%s]: "+fmt, append([]interface{}{proc.Name}, args...)...)
}

func dieIfError(err error) {
	if err != nil {
		log.Fatal("Error: ", err)
	}
}

func arraySearchStr(needle string, arr []string) int {
	for i, s := range arr {
		if s == needle {
			return i
		}
	}

	return -1
}

func parseSignal(sig string) (syscall.Signal, error) {
	switch strings.ToUpper(sig) {
	case "SIGINT":
		return syscall.SIGINT, nil
	case "SIGTERM":
		return syscall.SIGTERM, nil
	case "SIGKILL":
		return syscall.SIGKILL, nil
	case "SIGUSR1":
		return SIGUSR1, nil
	case "SIGUSR2":
		return SIGUSR2, nil
	case "SIGABRT":
		return syscall.SIGABRT, nil
	case "SIGQUIT":
		return syscall.SIGQUIT, nil
	case "SIGHUP":
		return syscall.SIGHUP, nil
	default:
		sigVal, err := strconv.Atoi(sig)
		if err != nil {
			return 0, err
		}

		return syscall.Signal(sigVal), nil
	}
}

func stripJSONComments(str string) string {
	lines := strings.Split(str, "\n")
	stripedLines := make([]string, len(lines))
	j := 0

	for _, line := range lines {

		// 去除空白后，如果以//开头则为注释行
		s := strings.TrimSpace(line)
		if len(s) > 2 && s[:2] == "//" {
			continue
		}

		stripedLines[j] = line
		j++
	}

	return strings.Join(stripedLines[:j], "\n")
}
