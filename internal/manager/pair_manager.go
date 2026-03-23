package manager

import (
	"encoding/json"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/bitxeno/atvloadly/internal/app"
	"github.com/bitxeno/atvloadly/internal/log"
	"github.com/bitxeno/atvloadly/internal/model"
	"github.com/gookit/event"
)

const directDeviceOutputPrefix = "ATVLOADLY_DIRECT_DEVICE="

type PairManager struct {
	outputStdout *pairOutputWriter
	outputStderr *pairOutputWriter

	stdin io.WriteCloser

	cancel context.CancelFunc
	em     *event.Manager
}

func NewPairManager() *PairManager {
	em := event.NewManager("output", event.UsePathMode)
	return &PairManager{
		outputStdout: newPairOutputWriter(em),
		outputStderr: newPairOutputWriter(em),

		em: em,
	}
}

type PairOptions struct {
	UDID string
	IP   string
}

func (t *PairManager) Start(ctx context.Context, options PairOptions) error {
	// set execute timeout 1 miniutes
	timeout := time.Minute
	ctx, cancel := context.WithTimeout(ctx, timeout)
	t.cancel = cancel

	cmd := t.newCommand(ctx, options)
	cmd.Dir = app.Config.Server.DataDir
	cmd.Env = GetRunEnvs()
	cmd.Stdout = t.outputStdout
	cmd.Stderr = t.outputStderr

	var err error
	t.stdin, err = cmd.StdinPipe()
	if err != nil {
		log.Err(err).Msg("Error creating stdin pipe: ")
		return err
	}
	defer t.stdin.Close()

	if err := cmd.Start(); err != nil {
		if err == context.DeadlineExceeded {
			_ = cmd.Process.Kill()
		}
		log.Err(err).Msg("Error start pair script.")
		return err
	}

	err = cmd.Wait()
	if err != nil {
		log.Err(err).Msgf("Error executing pair script. %s", t.ErrorLog())
		return err
	}

	if options.IP != "" {
		if directDevice, parseErr := t.parseDirectDevice(); parseErr == nil {
			UpsertDirectDevice(*directDevice)
		} else {
			return parseErr
		}
	}

	return nil
}

func (t *PairManager) newCommand(ctx context.Context, options PairOptions) *exec.Cmd {
	if options.IP != "" {
		return exec.CommandContext(ctx, "appletvremote", "pair", "--host", options.IP)
	}

	return exec.CommandContext(ctx, "idevicepair", "pair", "-u", options.UDID, "-w")
}

func (t *PairManager) Close() {
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	if t.em != nil {
		_ = t.em.CloseWait()
	}
}

func (t *PairManager) OnOutput(fn func(string)) {
	t.em.On("output", event.ListenerFunc(func(e event.Event) error {
		fn(e.Get("text").(string))
		return nil
	}))
}

func (t *PairManager) Write(p []byte) {
	if t.stdin == nil {
		return
	}
	_, _ = t.stdin.Write(p)
}

func (t *PairManager) ErrorLog() string {
	return t.outputStderr.String()
}

func (t *PairManager) OutputLog() string {
	return t.outputStdout.String()
}

type pairOutputWriter struct {
	data []byte
	em   *event.Manager
}

func newPairOutputWriter(em *event.Manager) *pairOutputWriter {
	return &pairOutputWriter{
		em: em,
	}
}

func (w *pairOutputWriter) Write(p []byte) (n int, err error) {
	w.data = append(w.data, p...)
	w.em.MustFire("output", event.M{"text": string(p)})

	n = len(p)
	return n, nil
}

func (w *pairOutputWriter) String() string {
	return string(w.data)
}

func (t *PairManager) parseDirectDevice() (*model.DirectDevice, error) {
	for _, line := range strings.Split(t.OutputLog(), "\n") {
		if !strings.HasPrefix(line, directDeviceOutputPrefix) {
			continue
		}

		var device model.DirectDevice
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, directDeviceOutputPrefix)), &device); err != nil {
			return nil, fmt.Errorf("failed to decode direct device metadata: %w", err)
		}
		return &device, nil
	}

	return nil, errors.New("direct device metadata not found in pairing output")
}

func ImportPairingFile(ip string, data []byte, override bool) error {
	// Check if the current system is macOS, if so, importing is not supported
	if runtime.GOOS == "darwin" {
		return errors.New("importing pairing file is not supported on macOS")
	}

	udid, err := checkPairingFile(ip, data)
	if err != nil {
		return fmt.Errorf("pairing file validation failed: %w", err)
	}

	log.Infof("Pairing file imported successfully: %s", udid)
	return nil
}

func checkPairingFile(ip string, data []byte) (string, error) {
	// Create a temporary file
	tmpFile, err := os.CreateTemp("", "pairing-*.plist")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpFilePath := tmpFile.Name()

	// Ensure the temporary file is deleted when the function exits
	defer os.Remove(tmpFilePath)

	// Write data to the temporary file
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return "", fmt.Errorf("failed to write temp file: %w", err)
	}
	tmpFile.Close()

	// Execute the check command
	output, err := ExecuteCommand("plumesign", "check", "pairing", "--save", "--ip", ip, "-f", tmpFilePath)
	if err != nil {
		return "", err
	}

	// Parse UDID
	re := regexp.MustCompile("UDID\\s+`([^`]+)`")
	matches := re.FindStringSubmatch(string(output))
	if len(matches) >= 2 {
		return matches[1], nil
	}

	return "", errors.New("UDID not found in output")
}
