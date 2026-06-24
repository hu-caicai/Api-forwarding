package system_setting

var ServerAddress = "http://localhost:3002"
var WorkerUrl = ""
var WorkerValidKey = ""
var WorkerAllowHttpImageRequestEnabled = false

func EnableWorker() bool {
	return WorkerUrl != ""
}
