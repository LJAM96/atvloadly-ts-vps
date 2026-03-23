package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/bitxeno/atvloadly/internal/app"
	"github.com/bitxeno/atvloadly/internal/log"
	"github.com/bitxeno/atvloadly/internal/model"
	"github.com/bitxeno/atvloadly/internal/utils"
	"github.com/gookit/event"
)

var ErrAccountInvalid = errors.New("account invalid")

type InstallManager struct {
	quietMode bool

	outputStdout *outputWriter

	stdin io.WriteCloser

	cancel              context.CancelFunc
	em                  *event.Manager
	ProvisioningProfile *model.MobileProvisioningProfile
}

type InstallOptions struct {
	UDID             string
	Account          string
	Password         string
	IpaPath          string
	RemoveExtensions bool
	RefreshMode      bool
}

func NewInstallManager() *InstallManager {
	em := event.NewManager("output", event.UsePathMode)
	return &InstallManager{
		quietMode:    true,
		outputStdout: newOutputWriter(em),

		em: em,
	}
}

func NewInteractiveInstallManager() *InstallManager {
	ins := NewInstallManager()
	ins.quietMode = false
	return ins
}

func (t *InstallManager) TryStart(ctx context.Context, opts InstallOptions) error {
	err := t.Start(ctx, opts)
	if err != nil {
		if t.IsAccountInvalid() {
			return fmt.Errorf("%s %s %w", t.ErrorLog(), err.Error(), ErrAccountInvalid)
		}

		if _, ok := GetDirectDeviceByUDID(opts.UDID); ok {
			return err
		}

		// AppleTV system has reboot/lockdownd sleep, try restart usbmuxd to fix
		// LOCKDOWN_E_MUX_ERROR / AFC_E_MUX_ERROR /
		ipaName := filepath.Base(opts.IpaPath)
		log.Infof("Try restarting usbmuxd to fix afc connect issue. %s", ipaName)
		if errmux := RestartUsbmuxd(); errmux == nil {
			// iPhone reconnect may take a while, wait some time
			time.Sleep(30 * time.Second)
			log.Infof("Restart usbmuxd complete, try install ipa again. %s", ipaName)
			err = t.Start(ctx, opts)
		}
	}
	return err
}

func (t *InstallManager) Start(ctx context.Context, opts InstallOptions) error {
	t.outputStdout.Reset()
	t.ProvisioningProfile = nil

	// set execute timeout 30 miniutes
	timeout := 30 * time.Minute
	ctx, cancel := context.WithTimeout(ctx, timeout)
	t.cancel = cancel

	provisionPath := t.GetMobileProvisionPath()
	defer func() {
		if _, err := os.Stat(provisionPath); err == nil {
			_ = os.Remove(provisionPath)
		}
	}()

	if directDevice, ok := GetDirectDeviceByUDID(opts.UDID); ok {
		return t.startDirectInstall(ctx, opts, provisionPath, *directDevice)
	}

	if err := CheckAfcServiceStatus(opts.UDID); err != nil {
		return fmt.Errorf("afc service not available: %w", err)
	}

	return t.startLegacyInstall(ctx, opts, provisionPath)
}

func (t *InstallManager) startLegacyInstall(ctx context.Context, opts InstallOptions, provisionPath string) error {
	args := []string{"sign", "--apple-id", "--register-and-install", "--output-provision", provisionPath, "--udid", opts.UDID, "-u", opts.Account, "-p", opts.IpaPath}
	if opts.RemoveExtensions {
		args = append(args, "--remove-extensions")
	}
	if opts.RefreshMode {
		args = append(args, "--refresh")
	}

	if err := t.runCommand(ctx, true, "plumesign", args...); err != nil {
		return err
	}

	t.loadProvisioningProfile(provisionPath)

	return nil
}

func (t *InstallManager) startDirectInstall(ctx context.Context, opts InstallOptions, provisionPath string, directDevice model.DirectDevice) error {
	if directDevice.Host == "" || directDevice.RemoteIdentifier == "" {
		return errors.New("direct device metadata is incomplete")
	}
	if err := t.ensureAccountDeviceRegistered(opts.Account, opts.UDID, directDevice.Name); err != nil {
		return err
	}

	signedIpaPath := filepath.Join(os.TempDir(), fmt.Sprintf("signed-%d.ipa", time.Now().UnixNano()))
	defer func() {
		if _, err := os.Stat(signedIpaPath); err == nil {
			_ = os.Remove(signedIpaPath)
		}
	}()

	signArgs := []string{"sign", "--apple-id", "--output", signedIpaPath, "--output-provision", provisionPath, "--udid", opts.UDID, "-u", opts.Account, "-p", opts.IpaPath}
	if opts.RemoveExtensions {
		signArgs = append(signArgs, "--remove-extensions")
	}
	if opts.RefreshMode {
		t.WriteLog("Direct device refresh uses a full re-sign before tunnel install.\n")
	}

	if err := t.runCommand(ctx, true, "plumesign", signArgs...); err != nil {
		return err
	}
	t.loadProvisioningProfile(provisionPath)

	installArgs := []string{"install", "--host", directDevice.Host, "--identifier", directDevice.RemoteIdentifier, "--package", signedIpaPath}
	if directDevice.TunnelPort > 0 {
		installArgs = append(installArgs, "--tunnel-port", fmt.Sprintf("%d", directDevice.TunnelPort))
	}

	if err := t.runCommand(ctx, false, "appletvremote", installArgs...); err != nil {
		t.updateDirectDeviceFromOutput()
		return err
	}
	t.updateDirectDeviceFromOutput()

	return nil
}

func (t *InstallManager) ensureAccountDeviceRegistered(account, udid, name string) error {
	devices, err := GetAccountDevices(account)
	if err == nil {
		for _, device := range devices {
			if strings.EqualFold(device.DeviceNumber, udid) {
				return nil
			}
		}
	}

	if name == "" {
		name = "Apple TV"
	}
	t.WriteLog(fmt.Sprintf("Registering device with Apple Developer account... %s\n", udid))
	if err := RegisterAccountDevice(account, udid, name); err != nil {
		return fmt.Errorf("register device failed: %w", err)
	}
	return nil
}

func (t *InstallManager) GetMobileProvisionPath() string {
	return path.Join(os.TempDir(), fmt.Sprintf("embedded.mobileprovision.%d", time.Now().UnixNano()))
}

func (t *InstallManager) CleanTempFiles(ipaPath string) {
	ipaName := filepath.Base(ipaPath)
	fileNameWithoutExt := strings.TrimSuffix(ipaName, filepath.Ext(ipaName))

	utils.RemoveAllFiles(filepath.Join(app.Config.Server.DataDir, "tmp"), fileNameWithoutExt+"*")
	utils.RemoveAllFiles(os.TempDir(), fileNameWithoutExt+"*")

	utils.RemoveAllFiles(os.TempDir(), "plume_stage*")
}

func (t *InstallManager) Close() {
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	if t.em != nil {
		_ = t.em.CloseWait()
	}
}

func (t *InstallManager) OnOutput(fn func(string)) {
	t.em.On("output", event.ListenerFunc(func(e event.Event) error {
		fn(e.Get("text").(string))
		return nil
	}))
}

func (t *InstallManager) Write(p []byte) {
	if t.stdin != nil {
		_, _ = t.stdin.Write(p)
	}
}

func (t *InstallManager) ErrorLog() string {
	data := t.outputStdout.String()
	if data == "" {
		return ""
	}

	var lines []string
	for _, l := range strings.Split(data, "\n") {
		if strings.HasPrefix(strings.ToLower(l), "error") {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}

func (t *InstallManager) IsAccountInvalid() bool {
	log := t.OutputLog()
	return strings.Contains(log, "plumesign account list") || strings.Contains(log, "Can't log-in") || strings.Contains(log, "DeveloperSession creation failed")
}

func (t *InstallManager) IsSuccess() bool {
	log := t.OutputLog()
	return strings.Contains(log, "Installation Succeeded") || strings.Contains(log, "Installation complete")
}

func (t *InstallManager) OutputLog() string {
	return t.outputStdout.String()
}

func (t *InstallManager) WriteLog(msg string) {
	_, _ = t.outputStdout.Write([]byte(msg))
}

func (t *InstallManager) SaveLog(id uint) {
	data := t.OutputLog()

	// Hide log password string
	// data = strings.Replace(data, v.Password, "******", -1)

	saveDir := filepath.Join(app.Config.Server.DataDir, "log")
	if err := os.MkdirAll(saveDir, os.ModePerm); err != nil {
		log.Error("failed to create directory :" + saveDir)
		return
	}

	path := filepath.Join(saveDir, fmt.Sprintf("task_%d.log", id))
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		log.Error("write log failed :" + path)
		return
	}
}

func (t *InstallManager) runCommand(ctx context.Context, interactive bool, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = app.Config.Server.DataDir
	cmd.Env = GetRunEnvs()
	cmd.Stdout = t.outputStdout
	cmd.Stderr = t.outputStdout

	if interactive {
		stdin, err := cmd.StdinPipe()
		if err != nil {
			log.Err(err).Msg("Error creating stdin pipe: ")
			return err
		}
		t.stdin = stdin
		defer func() {
			_ = stdin.Close()
			if t.stdin == stdin {
				t.stdin = nil
			}
		}()
	} else {
		t.stdin = nil
	}

	if err := cmd.Start(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			_ = cmd.Process.Kill()
			log.Err(err).Msgf("Installation exceeded %d-minute timeout limit. %s", 30, t.ErrorLog())
			return fmt.Errorf("Installation exceeded %d-minute timeout limit. %s", err.Error())
		}
		return err
	}

	return cmd.Wait()
}

func (t *InstallManager) loadProvisioningProfile(provisionPath string) {
	if provisionProfile, err := model.ParseMobileProvisioningProfileFile(provisionPath); err == nil {
		t.ProvisioningProfile = provisionProfile
	}
}

func (t *InstallManager) updateDirectDeviceFromOutput() {
	for _, line := range strings.Split(t.OutputLog(), "\n") {
		if !strings.HasPrefix(line, directDeviceOutputPrefix) {
			continue
		}

		var device model.DirectDevice
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, directDeviceOutputPrefix)), &device); err != nil {
			continue
		}

		UpsertDirectDevice(device)
	}
}

type outputWriter struct {
	data []byte
	em   *event.Manager
}

func newOutputWriter(em *event.Manager) *outputWriter {
	return &outputWriter{
		em: em,
	}
}

func (w *outputWriter) Write(p []byte) (n int, err error) {
	w.data = append(w.data, p...)
	w.em.MustFire("output", event.M{"text": string(p)})

	n = len(p)
	return n, nil
}

func (w *outputWriter) String() string {
	return string(w.data)
}

func (w *outputWriter) Reset() {
	w.data = []byte{}
}
