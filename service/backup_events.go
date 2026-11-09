package service

import (
	"context"
	"fmt"

	"github.com/ReCasaOS/CasaOS-AppManagement/common"
)

// What a backup or a restore says while it runs.
//
// "Returns as soon as the copy has started" was the whole of the feedback: an
// app of two hundred gigabytes copied for an hour with nothing on screen, and
// the History tab said so afterwards. The four events here are what an install
// already says, and the dashboard shows them on the same kind of card.

// backupEventContext is a context carrying what every event of one run needs
// to say: the app, where it is going or coming from, which run, and whether
// this is a backup or a restore. Built once and passed down, so the store
// lookup for the title and icon happens once too.
func backupEventContext(ctx context.Context, app *ComposeApp, destination, stamp, kind string) context.Context {
	properties := map[string]string{}
	for key, value := range common.PropertiesFromContext(ctx) {
		properties[key] = value
	}

	properties[common.PropertyTypeAppName.Name] = app.Name
	properties[common.PropertyTypeBackupDestination.Name] = destination
	properties[common.PropertyTypeBackupStamp.Name] = stamp
	properties[common.PropertyTypeBackupKind.Name] = kind

	// best effort: an app the catalogue does not know has no title and no icon,
	// and the dashboard falls back to the name
	if err := app.UpdateEventPropertiesFromStoreInfo(properties); err != nil {
		delete(properties, common.PropertyTypeAppTitle.Name)
	}

	return common.WithProperties(ctx, properties)
}

func publishBackupBegin(ctx context.Context) {
	PublishEventWrapper(ctx, common.EventTypeBackupBegin, nil)
}

// publishBackupProgress says which operation is under way, out of how many.
func publishBackupProgress(ctx context.Context, done, total int, current string) {
	percent := 0
	if total > 0 {
		percent = done * 100 / total
	}

	PublishEventWrapper(ctx, common.EventTypeBackupProgress, map[string]string{
		common.PropertyTypeAppProgress.Name: fmt.Sprintf("%d", percent),
		common.PropertyTypeMessage.Name:     current,
	})
}

// publishBackupFraction says how far the whole run has got, between 0 and 1, while an operation
// copies: the engines' counters make it move inside one folder, not only between two.
func publishBackupFraction(ctx context.Context, fraction float64, current string) {
	PublishEventWrapper(ctx, common.EventTypeBackupProgress, map[string]string{
		common.PropertyTypeAppProgress.Name: fmt.Sprintf("%d", int(fraction*100)),
		common.PropertyTypeMessage.Name:     current,
	})
}

func publishBackupEnd(ctx context.Context) {
	PublishEventWrapper(ctx, common.EventTypeBackupEnd, nil)
}

func publishBackupError(ctx context.Context, err error) {
	PublishEventWrapper(ctx, common.EventTypeBackupError, map[string]string{
		common.PropertyTypeMessage.Name: err.Error(),
	})
}
