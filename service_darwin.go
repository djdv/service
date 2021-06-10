// Copyright 2015 Daniel Theophanes.
// Use of this source code is governed by a zlib-style
// license that can be found in the LICENSE file.

package service

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"text/template"
	"time"
)

const (
	maxPathSize = 32 * 1024
	version     = "darwin-launchd"
	controlCmd  = "launchctl"
)

type darwinSystem struct{}

func (darwinSystem) String() string {
	return version
}
func (darwinSystem) Detect() bool {
	return true
}
func (darwinSystem) Interactive() bool {
	return interactive
}
func (darwinSystem) New(i Interface, c *Config) (Service, error) {
	s := &darwinLaunchdService{
		i:      i,
		Config: c,

		userService: c.Option.bool(optionUserService, optionUserServiceDefault),
	}

	return s, nil
}

func init() {
	ChooseSystem(darwinSystem{})
}

var interactive = false

func init() {
	var err error
	interactive, err = isInteractive()
	if err != nil {
		panic(err)
	}
}

func isInteractive() (bool, error) {
	// TODO: The PPID of Launchd is 1. The PPid of a service process should match launchd's PID.
	return os.Getppid() != 1, nil
}

const (
	optionSocketsKey          = "SocketsKey"
	optionSockType            = "SockType"
	optionSockPassive         = "SockPassive"
	optionSockNodeName        = "SockNodeName"
	optionSockServiceName     = "SockServiceName"
	optionSockFamily          = "SockFamily"
	optionSockProtocol        = "SockProtocol"
	optionSockPathName        = "SockPathName"
	optionSecureSocketWithKey = "SecureSocketWithKey"
	optionSockPathOwner       = "SockPathOwner"
	optionSockPathGroup       = "SockPathGroup"
	optionSockPathMode        = "SockPathMode"
	optionBonjour             = "Bonjour"
	optionMulticastGroup      = "MulticastGroup"
)

type darwinLaunchdService struct {
	i Interface
	*Config

	userService bool
}

func (s *darwinLaunchdService) String() string {
	if len(s.DisplayName) > 0 {
		return s.DisplayName
	}
	return s.Name
}

func (s *darwinLaunchdService) Platform() string {
	return version
}

func (s *darwinLaunchdService) getHomeDir() (string, error) {
	u, err := user.Current()
	if err == nil {
		return u.HomeDir, nil
	}

	// alternate methods
	homeDir := os.Getenv("HOME") // *nix
	if homeDir == "" {
		return "", errors.New("User home directory not found.")
	}
	return homeDir, nil
}

func (s *darwinLaunchdService) getServiceFilePath() (string, error) {
	serviceFile := s.Name + ".plist"
	if s.userService {
		homeDir, err := s.getHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(homeDir, "/Library/LaunchAgents/", serviceFile), nil
	}
	return filepath.Join("/Library/LaunchDaemons/", serviceFile), nil
}

func (s *darwinLaunchdService) template() *template.Template {
	functions := template.FuncMap{
		"bool": func(v bool) string {
			if v {
				return "true"
			}
			return "false"
		},
	}

	customConfig := s.Option.string(optionLaunchdConfig, "")

	if customConfig != "" {
		return template.Must(template.New("").Funcs(functions).Parse(customConfig))
	} else {
		return template.Must(template.New("").Funcs(functions).Parse(launchdConfig))
	}
}

func (s *darwinLaunchdService) Install() error {
	confPath, err := s.getServiceFilePath()
	if err != nil {
		return err
	}
	_, err = os.Stat(confPath)
	if err == nil {
		return fmt.Errorf("Init already exists: %s", confPath)
	}

	if s.userService {
		// Ensure that ~/Library/LaunchAgents exists.
		err = os.MkdirAll(filepath.Dir(confPath), 0700)
		if err != nil {
			return err
		}
	}

	f, err := os.Create(confPath)
	if err != nil {
		return err
	}
	defer f.Close()

	path, err := s.execPath()
	if err != nil {
		return err
	}

	type socketsProperty struct {
		SocketKey,
		Type string
		Passive bool
		NodeName,
		ServiceName,
		Family,
		Protocol,
		PathName,
		SecureWithKey string
		PathOwner,
		PathGroup,
		PathMode int
		Bonjour,
		MulticastGroup string
	}

	var (
		maybeSocketsProperty *socketsProperty
		spDefault            = socketsProperty{Passive: true}
		sp                   = socketsProperty{
			SocketKey:      s.Option.string(optionSocketsKey, ""),
			Type:           s.Option.string(optionSockType, ""),
			Passive:        s.Option.bool(optionSockPassive, true),
			NodeName:       s.Option.string(optionSockNodeName, ""),
			ServiceName:    s.Option.string(optionSockServiceName, ""),
			Family:         s.Option.string(optionSockFamily, ""),
			Protocol:       s.Option.string(optionSockProtocol, ""),
			PathName:       s.Option.string(optionSockPathName, ""),
			SecureWithKey:  s.Option.string(optionSecureSocketWithKey, ""),
			PathOwner:      s.Option.int(optionSockPathOwner, 0),
			PathGroup:      s.Option.int(optionSockPathGroup, 0),
			PathMode:       s.Option.int(optionSockPathMode, 0),
			Bonjour:        s.Option.string(optionBonjour, ""),
			MulticastGroup: s.Option.string(optionMulticastGroup, ""),
		}
	)
	if sp != spDefault {
		maybeSocketsProperty = &sp
	}

	var to = &struct {
		*Config
		Path string

		KeepAlive, RunAtLoad bool
		SessionCreate        bool
		StandardOut          bool
		StandardError        bool
		Sockets              *socketsProperty
	}{
		Config:        s.Config,
		Path:          path,
		KeepAlive:     s.Option.bool(optionKeepAlive, optionKeepAliveDefault),
		RunAtLoad:     s.Option.bool(optionRunAtLoad, optionRunAtLoadDefault),
		SessionCreate: s.Option.bool(optionSessionCreate, optionSessionCreateDefault),
		Sockets:       maybeSocketsProperty,
	}

	return s.template().Execute(f, to)
}

func (s *darwinLaunchdService) Uninstall() error {
	s.Stop()

	confPath, err := s.getServiceFilePath()
	if err != nil {
		return err
	}
	return os.Remove(confPath)
}

func (s *darwinLaunchdService) Status() (Status, error) {
	var (
		// Scan output for a valid PID.
		re        = regexp.MustCompile(`"PID" = ([0-9]+);`)
		isRunning = func(out string) bool {
			matches := re.FindStringSubmatch(out)
			return len(matches) == 2 // `PID = 1234`
		}
		canIgnore = func(err error) bool {
			return err == nil ||
				strings.Contains(err.Error(),
					"Could not find service")
		}
	)

	// Get output from `list`.
	controlArgs := []string{"list", s.Name}
	_, out, err := runWithOutput(controlCmd, controlArgs...)
	if !canIgnore(err) {
		return StatusUnknown, err
	}
	if isRunning(out) {
		return StatusRunning, nil
	}

	// `list` will always return a "job" entry if the job is "loaded"
	// (see launchd documentation for keyword semantics)
	// We'll check if the plist actually exist to determine if it's installed or not.
	// And since it's not running, we can assume it's stopped (but still installed).
	confPath, err := s.getServiceFilePath()
	if err != nil {
		return StatusUnknown, err
	}
	if _, err := os.Stat(confPath); err == nil {
		return StatusStopped, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return StatusUnknown, err
	}

	// Otherwise assume the service is not installed.
	return StatusUnknown, ErrNotInstalled
}

func (s *darwinLaunchdService) Start() error {
	confPath, err := s.getServiceFilePath()
	if err != nil {
		return err
	}
	if err := run(controlCmd, "load", confPath); err != nil {
		return err
	}
	if !s.Option.bool(optionRunAtLoad, optionRunAtLoadDefault) {
		return run(controlCmd, "start", s.Name)
	}
	return nil
}
func (s *darwinLaunchdService) Stop() error {
	confPath, err := s.getServiceFilePath()
	if err != nil {
		return err
	}
	return run(controlCmd, "unload", confPath)
}
func (s *darwinLaunchdService) Restart() error {
	err := s.Stop()
	if err != nil {
		return err
	}
	time.Sleep(50 * time.Millisecond)
	return s.Start()
}

func (s *darwinLaunchdService) Run() error {
	var err error

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

func (s *darwinLaunchdService) Logger(errs chan<- error) (Logger, error) {
	if interactive {
		return ConsoleLogger, nil
	}
	return s.SystemLogger(errs)
}
func (s *darwinLaunchdService) SystemLogger(errs chan<- error) (Logger, error) {
	return newSysLogger(s.Name, errs)
}

var launchdConfig = `<?xml version='1.0' encoding='UTF-8'?>
<!DOCTYPE plist PUBLIC "-//Apple Computer//DTD PLIST 1.0//EN"
"http://www.apple.com/DTDs/PropertyList-1.0.dtd" >
<plist version='1.0'>
<dict>
	<key>Label</key>
	<string>{{html .Name}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{html .Path}}</string>
	{{- range .Config.Arguments -}}
		<string>{{html .}}</string>
	{{- end}}
	</array>
	{{- if .UserName -}}<key>UserName</key>
	<string>{{html .UserName}}</string>{{end}}
	{{- if .ChRoot -}}<key>RootDirectory</key>
	<string>{{html .ChRoot}}</string>{{end}}
	{{- if .WorkingDirectory -}}<key>WorkingDirectory</key>
	<string>{{html .WorkingDirectory}}</string>{{end}}
	<key>SessionCreate</key>
	<{{bool .SessionCreate}}/>
	<key>KeepAlive</key>
	<{{bool .KeepAlive}}/>
	<key>RunAtLoad</key>
	<{{bool .RunAtLoad}}/>
	<key>Disabled</key>
	<false/>
	<key>StandardOutPath</key>
	<string>/usr/local/var/log/{{html .Name}}.out.log</string>
	<key>StandardErrorPath</key>
	<string>/usr/local/var/log/{{html .Name}}.err.log</string>
	{{- with .Sockets -}}<key>Sockets</key>
	<dict>
		<key>{{- if .SocketKey -}}{{.SocketKey}}{{else}}user defined{{end}}</key>
		<dict>
			<key>SockPassive</key>
			<{{bool .Passive}}/>
			{{if .Type -}}<key>SockType</key>
			<string>{{.Type -}}</string>{{end}}
			{{if .NodeName -}}<key>SockNodeName</key>
			<string>{{.NodeName -}}</string>{{end}}
			{{if .ServiceName -}}<key>SockServiceName</key>
			<string>{{.ServiceName -}}</string>{{end}}
			{{if .Family -}}<key>SockFamily</key>
			<string>{{.Family -}}</string>{{end}}
			{{if .Protocol -}}<key>SockProtocol</key>
			<string>{{.Protocol -}}</string>{{end}}
			{{if .PathName -}}<key>SockPathName</key>
			<string>{{.PathName -}}</string>{{end}}
			{{if .SecureWithKey -}}<key>SecureSocketWithKey</key>
			<string>{{.SecureWithKey -}}</string>{{end}}
			{{if .PathOwner -}}<key>SockPathOwner</key>
			<integer>{{.PathOwner -}}</integer>{{end}}
			{{if .PathGroup -}}<key>SockPathGroup</key>
			<integer>{{.PathGroup -}}</integer>{{end}}
			{{if .PathMode -}}<key>SockPathMode</key>
			<integer>{{.PathMode -}}</integer>{{end}}
			{{if .Bonjour -}}<key>Bonjour</key>
			<string>{{.Bonjour -}}</string>{{end}}
			{{if .MulticastGroup -}}<key>MulticastGroup</key>
			<string>{{.MulticastGroup -}}</string>{{end}}
		</dict>
	</dict>{{end}}
</dict>
</plist>
`
