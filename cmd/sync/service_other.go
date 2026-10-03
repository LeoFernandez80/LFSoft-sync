//go:build !windows

package main

func isWindowsService() bool {
	return false
}

func runAsService(_ string) error {
	return nil
}
