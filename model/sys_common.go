package model

import "time"

type CommonModel struct {
	RuntimePath string
}

type APPModel struct {
	LogPath      string
	LogSaveName  string
	LogFileExt   string
	AppStorePath string
	AppsPath     string
	DataPath     string

	// UpWaitTimeout bounds how long an app is given to report itself running or
	// healthy after it is started. Zero (the key absent) keeps the built-in default.
	// A duration with a unit: "5m", "20m".
	UpWaitTimeout time.Duration

	// The apps' logs: where they are kept, for how long and up to what size, and whether
	// the containers of no app are kept too.
	LogsPath          string
	LogsRetentionDays int
	LogsMaxSizeMB     int
	LogsIncludeOthers bool
}

type ServerModel struct {
	AppStoreList []string `ini:"appstore,,allowshadow"`
}

type GlobalModel struct {
	OpenAIAPIKey string
}

type CasaOSGlobalVariables struct {
	AppChange bool
}
