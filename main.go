package main

import (
	// "os"

	// "github.com/Techloopio/extractor_tool/autoupdater"
	"github.com/Techloopio/extractor_tool/cmd"
)

var (
	version = "v9.9.9" // Version of the file. E.g v0.9.6. This is set during build time.
)

func main() {
	cmd.Version = version

	// Auto update
	// skipUpdate := false
	// for _, arg := range os.Args {
	// 	if arg == "--skip_update" {
	// 		skipUpdate = true
	// 		break
	// 	}
	// }
	// if !skipUpdate {
	// 	au := autoupdater.NewAutoUpdater(version)
	// 	au.CheckUpdates()
	// }

	cmd.Execute()
}
