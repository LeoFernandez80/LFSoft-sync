package main

import (
	"os"
	"strings"

	"golang.org/x/sys/windows/svc"
)

func isWindowsService() bool {
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		return false
	}
	return isSvc
}

type syncService struct {
	stop chan struct{}
}

func (s *syncService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}

	s.stop = make(chan struct{})

	cfgPath := findConfigArg(os.Args[1:])
	if cfgPath == "" {
		cfgPath = findConfigArg(args)
	}
	if cfgPath == "" {
		cfgPath = "config/sync.yaml"
	}

	go runSync(cfgPath, "", false, s.stop)

	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for {
		c := <-r
		switch c.Cmd {
		case svc.Interrogate:
			changes <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			changes <- svc.Status{State: svc.StopPending}
			close(s.stop)
			return false, 0
		default:
			return false, 1
		}
	}
}

func findConfigArg(args []string) string {
	for i, arg := range args {
		if arg == "--config" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(arg, "--config=") {
			return strings.TrimPrefix(arg, "--config=")
		}
	}
	return ""
}

func runAsService(name string) error {
	return svc.Run(name, &syncService{})
}
