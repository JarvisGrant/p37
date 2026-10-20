//go:build !linux

package health

import "errors"

var errNotApplicable = errors.New("health: not applicable")

func overlaySupported() bool { return false }

func fsType(string) (string, error) { return "", errNotApplicable }

func inotifyLimits() (int, int, error) { return 0, 0, errNotApplicable }
