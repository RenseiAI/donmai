//go:build !darwin

package daemon

import "github.com/RenseiAI/donmai/afclient"

func daemonProcessPriorityStatus() afclient.DaemonProcessPriorityStatus {
	return afclient.DaemonProcessPriorityStatus{
		Supported: false,
		Mode:      "default",
		QoSClass:  "default",
		IOPolicy:  "default",
		Warning:   "process-priority self-check is only available on macOS launchd installs",
	}
}
