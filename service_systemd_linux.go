// Copyright 2015 Daniel Theophanes.
// Use of this source code is governed by a zlib-style
// license that can be found in the LICENSE file.

package service

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"text/template"
)

const (
	optionSystemdSocket          = "SystemdSocket"
	optionListenStream           = "ListenStream"
	optionListenDatagram         = "ListenDatagram"
	optionListenSequentialPacket = "ListenSequentialPacket"
)

type systemdUnit struct {
	name,
	path string
	data interface {
		io.Reader
		io.WriterTo
	}
}

func isSystemd() bool {
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		return true
	}
	if _, err := os.Stat("/proc/1/comm"); err == nil {
		filerc, err := os.Open("/proc/1/comm")
		if err != nil {
			return false
		}
		defer filerc.Close()

		buf := new(bytes.Buffer)
		buf.ReadFrom(filerc)
		contents := buf.String()

		if strings.Trim(contents, " \r\n") == "systemd" {
			return true
		}
	}
	return false
}

type systemd struct {
	i        Interface
	platform string
	*Config
}

func newSystemdService(i Interface, platform string, c *Config) (Service, error) {
	s := &systemd{
		i:        i,
		platform: platform,
		Config:   c,
	}

	return s, nil
}

func (s *systemd) String() string {
	if len(s.DisplayName) > 0 {
		return s.DisplayName
	}
	return s.Name
}

func (s *systemd) Platform() string {
	return s.platform
}

func (s *systemd) configPath() (string, error) {
	if !s.isUserService() {
		return "/etc/systemd/system", nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	systemdUserDir := filepath.Join(homeDir, ".config/systemd/user")
	if err := os.MkdirAll(systemdUserDir, os.ModePerm); err != nil {
		return "", err
	}
	return systemdUserDir, nil
}

func (s *systemd) unitName() string {
	return s.Config.Name + ".service"
}

func (s *systemd) getSystemdVersion() int64 {
	_, out, err := runWithOutput("systemctl", "--version")
	if err != nil {
		return -1
	}

	re := regexp.MustCompile(`systemd ([0-9]+)`)
	matches := re.FindStringSubmatch(out)
	if len(matches) != 2 {
		return -1
	}

	v, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return -1
	}

	return v
}

func (s *systemd) hasOutputFileSupport() bool {
	defaultValue := true
	version := s.getSystemdVersion()
	if version == -1 {
		return defaultValue
	}

	if version < 236 {
		return false
	}

	return defaultValue
}

func (s *systemd) isUserService() bool {
	return s.Option.bool(optionUserService, optionUserServiceDefault)
}

func (s *systemd) Install() error {
	units, err := s.units()
	if err != nil {
		return err
	}

	// Don't overwrite existing unit files.
	for _, unit := range units {
		if _, err := os.Lstat(unit.path); err == nil {
			return fmt.Errorf(`unit file already exists: "%s"`, unit.path)
		}
	}

	// Actual installation of unit files.
	for _, unit := range units {
		if err := createUnitFile(unit.path, unit.data); err != nil {
			return err
		}
	}

	// Enable the units which were just installed.
	for _, unit := range units {
		if err := s.run("enable", unit.name); err != nil {
			return err
		}
	}

	return nil
}

func createUnitFile(path string, data io.WriterTo) error {
	serviceFile, err := os.OpenFile(path,
		os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	if _, err := data.WriteTo(serviceFile); err != nil {
		err = fmt.Errorf(`write ("%s"): %w`, path, err)
		if cErr := serviceFile.Close(); cErr != nil {
			err = fmt.Errorf(`%w - close: %s`, err, cErr)
		}
		return err
	}
	if err = serviceFile.Close(); err != nil {
		err = fmt.Errorf(`close ("%s"): %w`, path, err)
	}
	return err
}

func (s *systemd) Uninstall() error {
	units, err := s.units()
	if err != nil {
		return err
	}
	for _, unit := range units {
		if err := s.run("disable", unit.name); err != nil {
			return err
		}
	}

	for _, unit := range units {
		if err := os.Remove(unit.path); err != nil {
			return err
		}
	}

	return nil
}

func (s *systemd) Logger(errs chan<- error) (Logger, error) {
	if system.Interactive() {
		return ConsoleLogger, nil
	}
	return s.SystemLogger(errs)
}
func (s *systemd) SystemLogger(errs chan<- error) (Logger, error) {
	return newSysLogger(s.Name, errs)
}

func (s *systemd) Run() (err error) {
	err = s.i.Start(s)
	if err != nil {
		return err
	}

	s.Option.funcSingle(optionRunWait, func() {
		var sigChan = make(chan os.Signal, 3)
		signal.Notify(sigChan, syscall.SIGTERM, os.Interrupt)
		<-sigChan
	})()

	return s.i.Stop(s)
}

func (s *systemd) Status() (Status, error) {
	exitCode, out, err := runWithOutput("systemctl", "is-active", s.unitName())
	if exitCode == 0 && err != nil {
		return StatusUnknown, err
	}

	switch {
	case strings.HasPrefix(out, "active"):
		return StatusRunning, nil
	case strings.HasPrefix(out, "inactive"):
		// inactive can also mean its not installed, check unit files
		exitCode, out, err := runWithOutput("systemctl", "list-unit-files", "-t", "service", s.unitName())
		if exitCode == 0 && err != nil {
			return StatusUnknown, err
		}
		if strings.Contains(out, s.Name) {
			// unit file exists, installed but not running
			return StatusStopped, nil
		}
		// no unit file
		return StatusUnknown, ErrNotInstalled
	case strings.HasPrefix(out, "activating"):
		return StatusRunning, nil
	case strings.HasPrefix(out, "failed"):
		return StatusUnknown, errors.New("service in failed state")
	default:
		return StatusUnknown, ErrNotInstalled
	}
}

func (s *systemd) Start() error {
	return s.runAction("start")
}

func (s *systemd) Stop() error {
	return s.runAction("stop")
}

func (s *systemd) Restart() error {
	return s.runAction("restart")
}

func (s *systemd) run(action string, args ...string) error {
	if s.isUserService() {
		return run("systemctl", append([]string{action, "--user"}, args...)...)
	}
	return run("systemctl", append([]string{action}, args...)...)
}

func (s *systemd) runAction(action string) error {
	if err := s.run(action, s.unitName()); err != nil {
		return err
	}
	if s.hasSocketArguments() {
		// TODO: either follow the method convention above
		// or just don't and obviate all of this with the coreos lib.
		return s.run(action, s.Name+".socket")
	}
	return nil
}

func (s *systemd) serviceUnit() (*bytes.Buffer, error) {
	execPath, err := s.execPath()
	if err != nil {
		return nil, err
	}
	var (
		unitfileBuffer      = new(bytes.Buffer)
		serviceTemplateData = &struct {
			*Config
			Path                 string
			HasOutputFileSupport bool
			ReloadSignal         string
			PIDFile              string
			LimitNOFILE          int
			Restart              string
			SuccessExitStatus    string
			LogOutput            bool
		}{
			s.Config,
			execPath,
			s.hasOutputFileSupport(),
			s.Option.string(optionReloadSignal, ""),
			s.Option.string(optionPIDFile, ""),
			s.Option.int(optionLimitNOFILE, optionLimitNOFILEDefault),
			s.Option.string(optionRestart, "always"),
			s.Option.string(optionSuccessExitStatus, ""),
			s.Option.bool(optionLogOutput, optionLogOutputDefault),
		}
		templateErr = template.Must(template.New(
			"systemd-unit",
		).Funcs(
			tf,
		).Parse(
			systemdServiceScript,
		)).Execute(
			unitfileBuffer,
			serviceTemplateData,
		)
	)
	if templateErr != nil {
		return nil, templateErr
	}
	return unitfileBuffer, nil
}

const systemdServiceScript = `[Unit]
Description={{.Description}}
ConditionFileIsExecutable={{.Path|cmdEscape}}
{{range $dep := .Dependencies}}
{{$dep}} {{end}}

[Service]
StartLimitInterval=5
StartLimitBurst=10
ExecStart={{.Path|cmdEscape}}{{range .Arguments}} {{.|cmd}}{{end}}
{{if .ChRoot}}RootDirectory={{.ChRoot|cmd}}{{end}}
{{if .WorkingDirectory}}WorkingDirectory={{.WorkingDirectory|cmdEscape}}{{end}}
{{if .UserName}}User={{.UserName}}{{end}}
{{if .ReloadSignal}}ExecReload=/bin/kill -{{.ReloadSignal}} "$MAINPID"{{end}}
{{if .PIDFile}}PIDFile={{.PIDFile|cmd}}{{end}}
{{if and .LogOutput .HasOutputFileSupport -}}
StandardOutput=file:/var/log/{{.Name}}.out
StandardError=file:/var/log/{{.Name}}.err
{{- end}}
{{if gt .LimitNOFILE -1 }}LimitNOFILE={{.LimitNOFILE}}{{end}}
{{if .Restart}}Restart={{.Restart}}{{end}}
{{if .SuccessExitStatus}}SuccessExitStatus={{.SuccessExitStatus}}{{end}}
RestartSec=120
EnvironmentFile=-/etc/sysconfig/{{.Name}}

[Install]
WantedBy=multi-user.target
`

func (s *systemd) hasSocketArguments() bool {
	var (
		_, literal = s.Option[optionSystemdSocket]
		_, streams = s.Option[optionListenStream]
		_, grams   = s.Option[optionListenDatagram]
		_, packets = s.Option[optionListenSequentialPacket]
	)
	return literal || streams || grams || packets
}

func (s *systemd) units() ([]systemdUnit, error) {
	var (
		units         []systemdUnit
		confPath, err = s.configPath()
	)
	if err != nil {
		return nil, err
	}

	// Initialize the data sources for unit files.
	// If the service's config contains a unit definition,
	// its value will be used as the data source for the unit.
	// Otherwise, the source is composed from
	// type-relevant service settings in the service's config.
	for _, settings := range []struct {
		unitType,
		parameter string
		generator func() (*bytes.Buffer, error)
	}{
		{
			"service",
			optionSystemdScript,
			s.serviceUnit,
		},
		{
			"socket",
			optionSystemdSocket,
			s.socketUnit,
		},
	} {
		var (
			unitName = s.Config.Name + "." + settings.unitType
			unit     = systemdUnit{
				name: unitName,
				path: filepath.Join(confPath, unitName),
			}
		)
		if setting := s.Option.string(settings.parameter, ""); setting != "" {
			unit.data = strings.NewReader(setting)
		} else {
			switch dataInterface, err := settings.generator(); {
			case err != nil:
				return nil, err
			case dataInterface != nil:
				unit.data = dataInterface
			default:
				continue // service has no data for this unit type
			}
		}
		units = append(units, unit)
	}

	return units, nil
}

func (s *systemd) socketUnit() (*bytes.Buffer, error) {
	var (
		streams = s.Option.strings(optionListenStream, nil)
		grams   = s.Option.strings(optionListenDatagram, nil)
		packets = s.Option.strings(optionListenSequentialPacket, nil)
		haveAny = len(streams) != 0 ||
			len(grams) != 0 ||
			len(packets) != 0
	)
	if !haveAny {
		return nil, nil
	}

	var (
		socketfileBuffer   = new(bytes.Buffer)
		socketTemplateData = &struct {
			Streams,
			Datagrams,
			Packets []string
		}{
			streams,
			grams,
			packets,
		}
		templateErr = template.Must(template.New(
			"systemd-socket",
		).Funcs(
			tf,
		).Parse(
			systemdSocketScript,
		)).Execute(
			socketfileBuffer,
			socketTemplateData,
		)
	)
	if templateErr != nil {
		return nil, templateErr
	}
	return socketfileBuffer, nil
}

const systemdSocketScript = `[Socket]
{{range $stream := .Streams}}
ListenStream={{$stream}}
{{end}}
{{range $gram := .Datagrams}}
ListenDatagram={{$gram}}
{{end}}
{{range $packet := .Packets}}
ListenSequentialPacket={{$packet}}
{{end}}

[Install]
WantedBy=sockets.target
`
