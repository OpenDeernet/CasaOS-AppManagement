package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	v1 "github.com/ReCasaOS/CasaOS-AppManagement/service/v1"

	"github.com/ReCasaOS/CasaOS-AppManagement/codegen"
	"github.com/ReCasaOS/CasaOS-AppManagement/common"
	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/config"
	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/docker"
	"github.com/ReCasaOS/CasaOS-Common/external"
	"github.com/ReCasaOS/CasaOS-Common/utils"
	"github.com/ReCasaOS/CasaOS-Common/utils/file"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/CasaOS-Common/utils/port"
	"github.com/ReCasaOS/CasaOS-Common/utils/random"
	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	composeCmd "github.com/docker/compose/v5/cmd/compose"

	"github.com/docker/compose/v5/cmd/formatter"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/go-resty/resty/v2"
	"github.com/samber/lo"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

type ComposeApp codegen.ComposeApp

func (a *ComposeApp) StoreInfo(includeApps bool) (*codegen.ComposeAppStoreInfo, error) {
	ex, ok := a.Extensions[common.ComposeExtensionNameXCasaOS]
	if !ok {
		return nil, ErrComposeExtensionNameXCasaOSNotFound
	}

	var storeInfo codegen.ComposeAppStoreInfo
	if err := loader.Transform(ex, &storeInfo); err != nil {
		logger.Error("Transform store info fail", zap.Error(err))
		return nil, err
	}

	// Only when the file actually says so: a nil field means the compose file did not
	// mention it, which is not the same answer as `false`.
	if uncontrolled, declared := a.uncontrolled(); declared {
		storeInfo.IsUncontrolled = &uncontrolled
	}

	// locate main app
	if storeInfo.Main == nil || *storeInfo.Main == "" {
		// if main app is not specified, use the first app
		for _, name := range sortedServiceNames(a.Services) {
			app := a.App(name)
			storeInfo.Main = &app.Name
			break
		}
	}

	if storeInfo.Scheme == nil || *storeInfo.Scheme == "" {
		storeInfo.Scheme = lo.ToPtr(codegen.Http)
	}

	if includeApps {
		apps := map[string]codegen.AppStoreInfo{}

		for _, name := range sortedServiceNames(a.Services) {
			app := a.App(name)
			appStoreInfo, err := app.StoreInfo()
			if err != nil {
				if err == ErrComposeExtensionNameXCasaOSNotFound {
					logger.Info("App does not have x-casaos extension - skipping", zap.String("app", app.Name))
					continue
				}

				return nil, err
			}
			apps[app.Name] = appStoreInfo
		}

		storeInfo.Apps = &apps
	}

	return &storeInfo, nil
}

// MainServiceName is the service an app's interface is about: what `x-casaos.main`
// names, and otherwise the alphabetically first service, which is the rule StoreInfo
// has always applied when that key is absent.
//
// A stack somebody assembled by hand has no `x-casaos` at all, and gets the same
// fallback rather than no answer. The extension carries presentation; which service
// leads is a question about the compose file, and it has an answer either way.
func (a *ComposeApp) MainServiceName() string {
	if ex, ok := a.Extensions[common.ComposeExtensionNameXCasaOS]; ok {
		var storeInfo codegen.ComposeAppStoreInfo
		if err := loader.Transform(ex, &storeInfo); err == nil && storeInfo.Main != nil && *storeInfo.Main != "" {
			return *storeInfo.Main
		}
	}

	for _, name := range sortedServiceNames(a.Services) {
		return name
	}

	return ""
}

// IsUncontrolled reports whether the app declares `x-casaos.is_uncontrolled`.
//
// The read this replaces was one expression whose FIRST type assertion had no
// comma-ok:
//
//	Extensions[x].(map[string]interface{})[y].(bool)
//
// The guard above it tests whether the KEY is present, which is not the same as the
// value being a map: `x-casaos:` with nothing after it parses to a nil value under a
// present key, and asserting that to a map type panics -- taking down whichever
// service called it. The copy of this line in the app grid did exactly that to a whole
// host in v0.4.61.
//
// Indexing a nil map is fine in Go. Asserting a nil interface to a map type is not.
func (a *ComposeApp) IsUncontrolled() bool {
	uncontrolled, _ := a.uncontrolled()

	return uncontrolled
}

// uncontrolled also reports whether the compose file said anything at all, which
// StoreInfo keeps: it leaves the field nil for a file that did not mention it.
func (a *ComposeApp) uncontrolled() (value bool, declared bool) {
	extension, _ := a.Extensions[common.ComposeExtensionNameXCasaOS].(map[string]interface{})
	value, declared = extension[common.ComposeExtensionPropertyNameIsUncontrolled].(bool)

	return value, declared
}

func (a *ComposeApp) AuthorType() codegen.StoreAppAuthorType {
	storeInfo, err := a.StoreInfo(false)
	if err != nil {
		return codegen.Unknown
	}

	if strings.EqualFold(storeInfo.Author, storeInfo.Developer) {
		return codegen.Official
	}
	if strings.EqualFold(storeInfo.Author, common.ComposeAppAuthorCasaOSTeam) {
		return codegen.ByCasaos
	}

	return codegen.Community
}

// SupportsArchitecture reports whether the compose app runs on arch (a runtime.GOARCH value:
// amd64, arm64, arm). A missing or empty `architectures` list means every architecture,
// matching the UI (AppPanel.vue `unuseable`).
func (a *ComposeApp) SupportsArchitecture(arch string) bool {
	storeInfo, err := a.StoreInfo(false)
	if err != nil {
		return false
	}

	if storeInfo.Architectures == nil || len(*storeInfo.Architectures) == 0 {
		return true
	}

	return lo.Contains(*storeInfo.Architectures, arch)
}

func (a *ComposeApp) SetStoreAppID(storeAppID string) (string, bool) {
	// set store_app_id (by convention is the same as app name at install time if it does not exist)
	extension, ok := a.Extensions[common.ComposeExtensionNameXCasaOS]
	if !ok {
		logger.Info("compose app does not have x-casaos extension - might not be a compose app for CasaOS", zap.String("app", a.Name))
		return "", false
	}

	composeAppStoreInfo, ok := extension.(map[string]interface{})
	if !ok {
		logger.Info("compose app does not have valid x-casaos extension - might not be a compose app for CasaOS", zap.String("app", a.Name))
		return "", false
	}

	value, ok := composeAppStoreInfo[common.ComposeExtensionPropertyNameStoreAppID]
	if ok {
		currentStoreAppID, ok := value.(string)
		if ok {
			logger.Info("compose app already has store_app_id", zap.String("app", a.Name), zap.String("storeAppID", currentStoreAppID))
			return currentStoreAppID, true
		}
	}

	composeAppStoreInfo[common.ComposeExtensionPropertyNameStoreAppID] = storeAppID
	return storeAppID, true
}

func (a *ComposeApp) SetTitle(title, lang string) {
	if a.Extensions == nil {
		a.Extensions = make(map[string]interface{})
	}

	extension, ok := a.Extensions[common.ComposeExtensionNameXCasaOS]
	if !ok {
		extension = map[string]interface{}{}
		a.Extensions[common.ComposeExtensionNameXCasaOS] = extension
	}

	composeAppStoreInfo, ok := extension.(map[string]interface{})
	if !ok {
		logger.Info("compose app does not have valid x-casaos extension - might not be a compose app for CasaOS", zap.String("app", a.Name))
		return
	}

	if _, ok := composeAppStoreInfo[common.ComposeExtensionPropertyNameTitle]; !ok {
		composeAppStoreInfo[common.ComposeExtensionPropertyNameTitle] = map[string]string{}
	}

	titleMap, ok := composeAppStoreInfo[common.ComposeExtensionPropertyNameTitle].(map[string]string)
	if !ok {
		logger.Info("compose app does not have valid title map in its x-casaos extension - might not be a compose app for CasaOS", zap.String("app", a.Name))
		return
	}

	if _, ok := titleMap[lang]; !ok {
		titleMap[lang] = title
	}
}

// Update is what the update button does: the app is replaced in the background
// and the request returns at once; the events of the update say the rest.
func (a *ComposeApp) Update(ctx context.Context) error {
	end, err := Begin(a.Name, "update")
	if err != nil {
		return err
	}

	newComposeYAML, ctx, err := a.prepareUpdate(ctx)
	if err != nil {
		end()
		return err
	}

	go func() {
		defer end()
		_ = a.applyUpdate(ctx, newComposeYAML)
	}()

	return nil
}

// UpdateNow is Update waited for: the run of every app at once does them one
// after another (compose_update_all.go).
func (a *ComposeApp) UpdateNow(ctx context.Context) error {
	end, err := Begin(a.Name, "update")
	if err != nil {
		return err
	}
	defer end()

	newComposeYAML, ctx, err := a.prepareUpdate(ctx)
	if err != nil {
		return err
	}

	return a.applyUpdate(ctx, newComposeYAML)
}

// prepareUpdate is the compose file the update writes, and the context its events
// carry the app's name in.
func (a *ComposeApp) prepareUpdate(ctx context.Context) ([]byte, context.Context, error) {
	if len(a.ComposeFiles) <= 0 {
		return nil, ctx, ErrComposeFileNotFound
	}

	if len(a.ComposeFiles) > 1 {
		logger.Info("the app is made of several compose files: a save rewrites the first one, and every one of them still applies", zap.String("compose files", strings.Join(a.ComposeFiles, ",")))
	}

	// An app whose compose file carries no `x-casaos` at all, or one whose extension
	// names no store app, has no catalogue entry -- and that is an answer, not a
	// failure. It is the answer composeYAMLForUpdate is already written for: nil means
	// there is nothing to update this app from but the tags it already names, so the
	// update becomes a re-pull of those.
	//
	// Refusing here is what made `Check then update` fail on a stack somebody wrote by
	// hand -- with ``extension `x-casaos` not found``, or `store info not found` -- on
	// an app whose images really had moved and which the check had just said so about.
	// The grid offers that button on every compose app, so the refusal was reachable
	// from the menu of any of them.
	storeInfo, err := a.StoreInfo(true)
	if err != nil && !errors.Is(err, ErrComposeExtensionNameXCasaOSNotFound) {
		return nil, ctx, err
	}

	// nil means no catalogue entry, which is an answer rather than a failure
	var storeComposeApp *ComposeApp

	if storeInfo != nil && storeInfo.StoreAppID != nil && *storeInfo.StoreAppID != "" {
		if storeComposeApp, err = MyService.AppStoreManagement().ComposeApp(*storeInfo.StoreAppID); err != nil {
			return nil, ctx, err
		}
	}

	newComposeYAML, err := a.composeYAMLForUpdate(storeComposeApp)
	if err != nil {
		return nil, ctx, err
	}

	// prepare for message bus events
	eventProperties := map[string]string{common.PropertyTypeAppName.Name: a.Name}

	if err := a.UpdateEventPropertiesFromStoreInfo(eventProperties); err != nil {
		logger.Info("failed to update event properties from store info", zap.Error(err), zap.String("name", a.Name))
	}

	common.SetProperties(ctx, eventProperties)

	return newComposeYAML, ctx, nil
}

// applyUpdate replaces the app with the compose file given and says so on the
// bus, begin to end, error included.
func (a *ComposeApp) applyUpdate(ctx context.Context, newComposeYAML []byte) error {
	go PublishEventWrapper(ctx, common.EventTypeAppUpdateBegin, nil)

	defer PublishEventWrapper(ctx, common.EventTypeAppUpdateEnd, nil)

	MyService.AppStoreManagement().StartUpgrade(a.Name)
	defer MyService.AppStoreManagement().FinishUpgrade(a.Name)

	before := mainImage(a)
	if err := a.PullAndApply(ctx, newComposeYAML); err != nil {
		go PublishEventWrapper(ctx, common.EventTypeAppUpdateError, map[string]string{
			common.PropertyTypeMessage.Name: err.Error(),
		})

		logger.Error("failed to update compose app", zap.Error(err), zap.String("name", a.Name))
		return err
	}
	// a is still the app before the update: the image after is the one in the files just applied
	after := ""
	if updated, err := LoadComposeAppFromConfigFile(a.Name, strings.Join(a.ComposeFiles, ",")); err == nil {
		after = mainImage(updated)
	}
	RecordAppEvent(a.Name, "updated", before, after, "")

	// The update applied. app:update-end is published whether this succeeded or
	// failed, so without this it says nothing at all and the dashboard has no
	// success to report -- which is why a finished update used to pass in silence.
	common.SetProperties(ctx, map[string]string{common.PropertyTypeAppUpdated.Name: "true"})

	// the app is no longer the one the cached answers were about, and an
	// `offered` left at true keeps badging an app that has just been updated
	forgetImageUpdates(a.Name)

	return nil
}

// composeYAMLForUpdate is the docker-compose.yml an update writes, which depends on
// where the app came from.
//
// An app with a catalogue entry takes that catalogue's images: it is what says which
// version the app should be on. An app with none -- an imported compose file, one
// written by hand -- has nothing to consult, so it keeps the images it already names
// and the update is the pull that follows: the tags stay the same, the digests they
// resolve to need not.
//
// A nil storeComposeApp means the catalogue holds nothing for this app.
//
// That, not the `is_uncontrolled` flag, is the test. The flag means something else: it
// is set when an app's main tag has been moved off the catalogue's, and an imported
// app is written with it FALSE (see IsNewComposeUncontrolled, which returns false when
// it finds no store entry). Keying on the flag left imported apps exactly as frozen as
// before and took the catalogue away from pinned apps, which are the only ones that
// have one.
//
// A git app's compose file is a tracked file of its repository, and CasaOS never writes
// one: the update writes back the bytes already there, and is the pull and the up that
// follow. Marshalled again, the file would read as modified to git, and every later
// deployment would refuse.
func (a *ComposeApp) composeYAMLForUpdate(storeComposeApp *ComposeApp) ([]byte, error) {
	if _, err := loadGitApp(a.Name); err == nil {
		return os.ReadFile(a.ComposeFiles[0])
	}

	if storeComposeApp == nil {
		return a.refreshedComposeYAML()
	}

	return a.updatedComposeYAML(storeComposeApp)
}

// refreshedComposeYAML is the file on disk, unchanged. It is built from the editing
// load rather than marshalled from a, for the reason spelled out on
// updatedComposeYAML: a is the runtime project, with `.env` already resolved, and
// marshalling it bakes every secret, published port and host path into the compose
// file and leaves `.env` dead.
func (a *ComposeApp) refreshedComposeYAML() ([]byte, error) {
	keys, err := a.EnvKeys()
	if err != nil {
		return nil, err
	}

	local, err := LoadComposeAppForEditing(a.Name, a.ComposeFiles[0], keys)
	if err != nil {
		return nil, err
	}

	return yaml.Marshal(local)
}

// updatedComposeYAML is the docker-compose.yml an update writes: the file on disk with the image of
// each service replaced by the store's. It is built from the editing load of that file, never from
// a itself: a is the runtime project (List), with `.env` resolved, and marshalling it bakes every
// secret, published port and host path into the compose file, leaving `.env` dead after the first
// App Store update. An unreadable `.env` is an error for the same reason.
func (a *ComposeApp) updatedComposeYAML(storeComposeApp *ComposeApp) ([]byte, error) {
	keys, err := a.EnvKeys()
	if err != nil {
		return nil, err
	}

	local, err := LoadComposeAppForEditing(a.Name, a.ComposeFiles[0], keys)
	if err != nil {
		return nil, err
	}

	// Services the local app has and the catalogue does not are the owner's own: a VPN
	// sidecar wired in by hand, a companion container. An update has nothing to say
	// about them, so it leaves them exactly as written rather than refusing -- refusing
	// meant that adding one container to a store app froze it forever, with nothing in
	// the interface saying why.
	//
	// The other direction is still a refusal: the catalogue has grown a service this
	// app does not run, and creating one is not something an update does yet. The error
	// names them and says why, because an interface that can only say `compose app not
	// match` leaves the owner with a button that fails and no reason.
	if storeAbsentOfLocal := servicesUpdateCannotCreate(local.Services, storeComposeApp.Services); len(storeAbsentOfLocal) > 0 {
		return nil, fmt.Errorf("%w: the app store version of %s adds services this app does not have (%s), and an update cannot create them",
			ErrComposeAppNotMatch, a.Name, strings.Join(storeAbsentOfLocal, ", "))
	}

	for name, service := range storeComposeApp.Services {
		localComposeAppService := local.Services[name] // present: storeAbsentOfLocal is empty

		if updateWritesStoreImage(localComposeAppService.Image, service.Image) {
			localComposeAppService.Image = service.Image
		}

		local.Services[name] = localComposeAppService
	}

	// the code is need by stable diffusion.
	removeRuntime(local)

	return yaml.Marshal(local)
}

// servicesUpdateCannotCreate names the services the catalogue has and this app does
// not. Both halves of an update read it: the write above refuses them, and the
// decision to offer an update at all has to refuse the same apps, or the dashboard
// badges a button whose only possible answer is a 500.
func servicesUpdateCannotCreate(local, store types.Services) []string {
	_, storeAbsentOfLocal := lo.Difference(sortedServiceNames(local), sortedServiceNames(store))

	return storeAbsentOfLocal
}

// updateWritesStoreImage reports whether an update would actually put the catalogue's
// image in the file for a service. Both halves of an update read it -- the write and
// the decision to offer one at all -- and they have to give the same answer, or the
// dashboard badges an app whose file the update then leaves exactly as it was, for
// ever.
//
// The question it really asks is: can a PULL alone bring this app up to date? If it
// can, the file keeps the reference it has. If it cannot, the catalogue's reference
// is the only way forward and gets written.
//
// A local reference that floats can be pulled, so it stays -- which is the fix for
// the case this was got wrong on: an app tracking `develop` had its tag replaced by
// whatever fixed version the catalogue named, and naming a stream is a choice rather
// than an omission.
//
// A digest is the opposite case, and reading it as "already exact, so leave it"
// gets it exactly backwards: `pull acme/app@sha256:0000` returns that same image for
// ever, so an app pinned to a digest can ONLY be updated by writing the catalogue's
// new reference over it. Leaving it alone freezes the app.
//
// The catalogue floating is the one thing that stops a write either way: a
// catalogue naming a stream says nothing about which version to run, so it cannot
// un-pin somebody who pinned.
//
// This used to read the STORE image alone, so the answer depended on the tag the
// catalogue happened to carry: a local `develop` survived a catalogue on `latest`
// and was overwritten by a catalogue on `1.2.3`. Not a rule -- an accident.
func updateWritesStoreImage(localImage, storeImage string) bool {
	return !referenceFloats(localImage) && !referenceFloats(storeImage)
}

// referenceFloats reports whether a pull can move this reference: a tag republished
// under the same name as new images are built.
//
// A digest does not float. It is exact, and that is precisely why it has to be
// rewritten to move.
func referenceFloats(image string) bool {
	_, tag := docker.ExtractImageAndTag(image)

	// ExtractImageAndTag answers "" for a digest reference
	if tag == "" {
		return false
	}

	return lo.Contains(common.NeedCheckDigestTags, tag)
}

// TODO rename the function to service and add error return value
func (a *ComposeApp) App(name string) *App {
	if name == "" {
		return nil
	}

	service, ok := a.Services[name]
	if !ok {
		return nil
	}
	service.Name = name
	app := App(service)

	return &app
}

func (a *ComposeApp) Apps() map[string]*App {
	apps := make(map[string]*App, len(a.Services))

	for _, name := range sortedServiceNames(a.Services) {
		apps[name] = a.App(name)
	}

	return apps
}

func sortedServiceNames(services types.Services) []string {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// MainService is the service MainServiceName names. Reading it out of StoreInfo
// instead meant a stack with no `x-casaos` had no main service at all, and every
// caller of this and of MainTag failed on it -- including the check that runs when
// the owner saves the settings of such an app.
func (a *ComposeApp) MainService() (*App, error) {
	name := a.MainServiceName()
	if name == "" {
		return nil, ErrMainServiceNotSpecified
	}

	return a.App(name), nil
}

func (a *ComposeApp) MainTag() (string, error) {
	mainService, err := a.MainService()
	if err != nil {
		return "", err
	}
	_, newTag := docker.ExtractImageAndTag(mainService.Image)

	return newTag, nil
}

func (a *ComposeApp) Containers(ctx context.Context) (map[string][]api.ContainerSummary, error) {
	service, dockerClient, err := apiService()
	if err != nil {
		return nil, err
	}
	defer dockerClient.Close()

	containers, err := service.Ps(ctx, a.Name, api.PsOptions{
		All: true,
	})
	if err != nil {
		return nil, err
	}

	// it is possible a `service` contains multiple containers.
	// See https://docs.docker.com/compose/compose-file/deploy/#replicas
	return lo.GroupBy(containers, func(container api.ContainerSummary) string {
		return container.Service
	}), nil
}

func (a *ComposeApp) Pull(ctx context.Context) error {
	serviceNames := pullServiceNames(ctx, a.Services)
	serviceNum := len(serviceNames)
	pulled := serviceNum > 0 // a failed pull is published, not returned

	for i, name := range serviceNames {
		app := a.Services[name]
		if err := func() error {
			go PublishEventWrapper(ctx, common.EventTypeImagePullBegin, map[string]string{
				common.PropertyTypeImageName.Name: app.Image,
			})

			defer PublishEventWrapper(ctx, common.EventTypeImagePullEnd, map[string]string{
				common.PropertyTypeImageName.Name: app.Image,
			})

			if err := docker.PullImage(ctx, app.Image, func(out io.ReadCloser) {
				pullImageProgress(ctx, out, "INSTALL", serviceNum, i+1)
			}); err != nil {
				go PublishEventWrapper(ctx, common.EventTypeImagePullError, map[string]string{
					common.PropertyTypeImageName.Name: app.Image,
					common.PropertyTypeMessage.Name:   err.Error(),
				})
				pulled = false
			}

			return nil
		}(); err != nil {
			return err
		}
	}

	if pulled {
		RecordAppEvent(a.Name, "image-pulled", "", mainImage(a), "")
	}
	return nil
}

// imageIsHere reports whether an image is on this box already, a var for the tests.
var imageIsHere = func(ctx context.Context, image string) bool {
	_, err := docker.Image(ctx, image)
	return err == nil
}

// pullServiceNames is what a pull fetches: pulledServiceNames, and the services with a build
// section whose image is not on this box. Such a service came from a compose file that builds
// and names an image a registry holds (`build:` and `image:` together, as a CI that pushes it
// does): installed here there is nothing to build it from, and no pull left its container with
// "No such image". An image that is here, built on this box or pulled before, is left as it is.
func pullServiceNames(ctx context.Context, services types.Services) []string {
	names := pulledServiceNames(services)
	for _, name := range sortedServiceNames(services) {
		if s := services[name]; s.Build != nil && s.Image != "" && !imageIsHere(ctx, s.Image) {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	return names
}

// pulledServiceNames is every service a pull fetches an image for, sorted. A service with a
// build section is left out: CasaOS never builds, its container runs the image built on this
// box, and a pull under the same name would replace that build with whatever a registry holds
// (compose's own `pull --ignore-buildable` leaves them out the same way).
func pulledServiceNames(services types.Services) []string {
	names := []string{}
	for _, name := range sortedServiceNames(services) {
		if services[name].Build == nil {
			names = append(names, name)
		}
	}
	return names
}

func (a *ComposeApp) injectEnvVariableToComposeApp() {
	for name, service := range a.Services {
		if service.Environment == nil {
			service.Environment = types.MappingWithEquals{}
		}
		for k, v := range config.Global {
			// if there is same name var declared in environment in compose yaml
			// we should not reassign a value to it.
			if service.Environment[k] == nil {
				service.Environment[k] = utils.Ptr(v)
			}
		}
		a.Services[name] = service
	}
}

// defaultUpWaitTimeout bounds how long we wait for an app's containers to become
// running-or-healthy, when `UpWaitTimeout` is not set in the `[app]` section of
// app-management.conf.
//
// The pull already happened by the time we get here, so this budget covers create,
// start and healthcheck convergence only. The other container waits in this service
// are 20s (waiting for an app to be fully exited) and the HTTP checks are 20-30s;
// the one long wait, 30 minutes, is the boot window for /DATA to mount, which is a
// different kind of waiting. Five minutes sits between them on purpose: enough for a
// dependency chain of healthchecks with 60s start_periods on a slow ARM box, short
// enough that a stack which will never converge is reported while the owner is still
// looking at the page. A box slower than that -- or a stack with a longer honest
// convergence -- sets the config key.
const defaultUpWaitTimeout = 5 * time.Minute

// errUpNotConfirmed marks a wait that ran out of time. It is not the same failure as
// compose refusing to start the app: the containers were created and started, only
// their running-or-healthy confirmation never arrived. It can be an app that is still
// converging, or a stack whose one-shot init container -- a db-migrate, a chown
// sidecar -- exits 0 and can never be "running or healthy", which is what compose
// waits for on every service nothing depends on with service_completed_successfully
// (getDependencyCondition, pkg/compose/start.go).
var errUpNotConfirmed = errors.New("not confirmed running or healthy")

// upWaitTimeout is the deadline in force, from `UpWaitTimeout` under `[app]`.
func upWaitTimeout() time.Duration {
	// ini leaves the field at zero for a missing key, and parses a unit-less number as
	// nanoseconds, so anything under a second is an unset key or a typo, not a choice.
	if d := config.AppInfo.UpWaitTimeout; d >= time.Second {
		return d
	}
	return defaultUpWaitTimeout
}

// waitForApp runs a compose operation that was asked to wait for containers, under a
// deadline, and reports a blown deadline as errUpNotConfirmed.
//
// Both halves are needed. api.StartOptions.Wait polls every 500ms for every service
// to be running-or-healthy and only wraps the context in a deadline of its own when
// WaitTimeout > 0 (pkg/compose/start.go), so without one it never gives up: a gluetun
// stack, whose services cannot start until the VPN container is healthy, parks here
// for ever and the caller never hears about it. And compose v2.27 swallows the
// cancellation -- waitDependencies returns nil for every service once the context is
// done (pkg/compose/convergence.go), and progress.Run passes that nil straight
// through -- so a wait that timed out comes back as success unless we look. That is
// also why passing our budget as StartOptions.WaitTimeout instead would not do: the
// deadline compose sets for itself is swallowed by the same nil.
func waitForApp(ctx context.Context, name string, up func(context.Context) error, settled func(context.Context) bool) error {
	timeout := upWaitTimeout()

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	upErr := up(waitCtx)

	if ctx.Err() != nil {
		// the caller gave up on us, not the other way around
		return fmt.Errorf("wait for compose app `%s` was cancelled: %w", name, ctx.Err())
	}

	if waitCtx.Err() != nil {
		// A blown deadline arrives two ways: compose swallows the cancellation and
		// returns nil, or it surfaces it as the error of whatever call was in flight
		// -- an inspect, a create. Same event, so the same answer. Classifying only
		// the first took the owner's edit away whenever the timeout happened to land
		// inside a call rather than between two polls.
		return fmt.Errorf("compose app `%s` was started but %w after %s", name, errUpNotConfirmed, timeout)
	}

	if upErr == nil {
		return nil
	}

	// compose waits for EVERY service to be running-or-healthy, which a one-shot init
	// container -- a db-migrate, a chown sidecar -- can never be: it exits 0 and
	// compose answers `container X exited (0)` within a poll or two, nowhere near the
	// deadline. That is not a failed apply, the stack is up and doing what its author
	// wrote. So ask the daemon what is actually there rather than reading compose's
	// message, which is prose and not an API.
	if settled != nil && settled(ctx) {
		return fmt.Errorf("compose app `%s` was started but %w: %w", name, errUpNotConfirmed, upErr)
	}

	return upErr
}

// everyContainerSettled reports whether the app's containers are all in a state
// somebody meant: running and not reporting itself unhealthy, or exited cleanly
// because finishing is what they were for. A non-zero exit, a restart loop or an
// unhealthy report is a failed apply, and the caller must put the previous definition
// back.
//
// Only ever asked after compose has created the containers from the definition in
// question, so what is running IS that definition. Asked any earlier it would answer
// about the previous one, which is still up until convergence replaces it.
//
// Not being able to ask is not a yes. An apply nobody can judge keeps the older
// behaviour, which is to roll back.
func (a *ComposeApp) everyContainerSettled(ctx context.Context) bool {
	containerLists, err := a.Containers(ctx)
	if err != nil {
		logger.Info("cannot tell whether the app settled, so treating the apply as failed",
			zap.Error(err), zap.String("name", a.Name))

		return false
	}

	seen := false

	for _, containers := range containerLists {
		for _, container := range containers {
			seen = true

			switch container.State {
			case "running":
				if strings.EqualFold(string(container.Health), "unhealthy") {
					return false
				}
			case "exited":
				if container.ExitCode != 0 {
					return false
				}
			default:
				// created, restarting, paused, removing, dead: none of them is a
				// state an app settles into
				return false
			}
		}
	}

	// no containers at all is not a stack that came up
	return seen
}

func (a *ComposeApp) Up(ctx context.Context, service api.Compose) error {
	return a.up(ctx, service, false)
}

// up starts the app. removeOrphans is for the rollback path and a git app's start only: the
// definition being replaced may have declared a service the restored one does not, and a
// container left over from it would keep running under no compose file at all. It stays off
// elsewhere, where an unknown container of this project is an adopted one, not a leftover.
// composeCreateStarter is the two halves of compose's Up, which is all `up` needs of
// api.Compose -- and narrow enough that the order it calls them in can be driven by a
// test, which is the whole point of keeping them apart.
type composeCreateStarter interface {
	Create(ctx context.Context, project *types.Project, options api.CreateOptions) error
	Start(ctx context.Context, projectName string, options api.StartOptions) error
}

func (a *ComposeApp) up(ctx context.Context, service composeCreateStarter, removeOrphans bool) error {
	a.injectEnvVariableToComposeApp()

	// Create and Start, rather than Up, which is exactly the two of them in one call
	// (pkg/compose/up.go: create, then start). Keeping them apart is what tells a stack
	// that came up and would not confirm from one compose never reached: everything
	// before convergence -- an image that does not exist, a network or a volume it
	// cannot create, a duplicate container_name -- fails HERE, with every container of
	// the previous definition still running and healthy. Asking `settled` about those
	// would read the old app as proof that the new definition worked and leave a compose
	// file the app cannot start from, with the backup already gone. Once Create returns,
	// the containers are the new definition, and only then does their state answer for
	// it.
	//
	// No deadline of our own here: the pull that precedes an apply has none either, and
	// Create's own work is bounded by ctx. UpWaitTimeout is the wait for an app to
	// report itself running or healthy AFTER it is started, which is what it says.
	if err := service.Create(ctx, (*codegen.ComposeApp)(a), api.CreateOptions{
		RemoveOrphans: removeOrphans,
	}); err != nil {
		logger.Error("failed to create compose app", zap.Error(err), zap.String("name", a.Name))
		return err
	}

	// no OnExit: compose only reads it when Start.Attach is set, and we never attach
	// (pkg/compose/up.go) -- CascadeStop here was decoration.
	if err := waitForApp(ctx, a.Name, func(ctx context.Context) error {
		// With the project, not only its name. Given none, compose rebuilds the project from
		// the containers' labels, which carry no post_start or pre_start hook, so a container
		// CasaOS created never ran its hooks. Every service has its containers here: they were
		// just created.
		return service.Start(ctx, a.Name, api.StartOptions{Project: (*codegen.ComposeApp)(a), Wait: true})
	}, a.everyContainerSettled); err != nil {
		logger.Error("failed to start original compose app", zap.Error(err), zap.String("name", a.Name))
		return err
	}
	return nil
}

func (a *ComposeApp) UpWithCheckRequire(ctx context.Context, service api.Compose) error {
	return a.upWithCheckRequire(ctx, service, false)
}

// upWithCheckRequire is UpWithCheckRequire, removing the project's orphans when asked. Only a
// git app's runtime asks: its repository's compose files are the whole app, so a container of a
// service they no longer name is what a switch or a rollback left of the version it replaced.
func (a *ComposeApp) upWithCheckRequire(ctx context.Context, service api.Compose, removeOrphans bool) error {
	// prepare source path for volumes if not exist
	for name, app := range a.Services {
		for _, volume := range app.Volumes {
			if _, ok := a.Volumes[volume.Source]; ok {
				// this is a internal volume, so skip.
				continue
			}

			path := volume.Source
			if err := file.IsNotExistMkDir(path); err != nil {
				go PublishEventWrapper(ctx, common.EventTypeContainerStartError, map[string]string{
					common.PropertyTypeMessage.Name: err.Error(),
				})
				return err
			}
		}

		// check if each required device exists
		deviceMapFiltered := []types.DeviceMapping{}
		for _, deviceMap := range app.Devices {
			devicePath := deviceMap.Source
			if file.CheckNotExist(devicePath) {
				logger.Info("device not found", zap.String("device", devicePath))
				continue
			}
			deviceMapFiltered = append(deviceMapFiltered, deviceMap)
		}
		app.Devices = deviceMapFiltered
		a.Services[name] = app
	}

	if err := a.up(ctx, service, removeOrphans); err != nil {
		go PublishEventWrapper(ctx, common.EventTypeContainerStartError, map[string]string{
			common.PropertyTypeMessage.Name: err.Error(),
		})
		return err
	}
	return nil
}

func (a *ComposeApp) PullAndApply(ctx context.Context, newComposeYAML []byte) error {
	return a.pullAndApply(ctx, newComposeYAML, nil)
}

// pullAndApply writes the new compose file and, when newEnv is not nil, the new `.env` (empty
// removes it), then pulls and (re)creates the app; both files are put back if that fails, so disk
// and running state never diverge.
func (a *ComposeApp) pullAndApply(ctx context.Context, newComposeYAML []byte, newEnv *[]byte) error {
	// backup current compose file
	currentComposeFile := a.ComposeFiles[0]

	backupComposeFile := currentComposeFile + "." + "bak"
	if err := file.CopySingleFile(currentComposeFile, backupComposeFile, ""); err != nil {
		return err
	}

	var oldEnv []byte // nil when there was none: WriteEnvFile(nil) removes
	if newEnv != nil {
		var err error
		if oldEnv, err = a.EnvFileText(); err != nil {
			return err
		}
	}

	// start compose app
	service, dockerClient, err := apiService()
	if err != nil {
		return err
	}
	defer dockerClient.Close()

	success := false

	defer func() {
		if !success {
			if err := file.CopySingleFile(backupComposeFile, currentComposeFile, ""); err != nil {
				logger.Error("failed to restore original compose file", zap.Error(err), zap.String("src", backupComposeFile), zap.String("dst", currentComposeFile))
				return
			}

			if newEnv != nil {
				if err := a.WriteEnvFile(oldEnv); err != nil {
					logger.Error("failed to restore original .env", zap.Error(err), zap.String("path", a.EnvFile()))
					return
				}
			}

			// with RemoveOrphans: the new file may have added a service, and its container
			// is not in the definition we just restored
			if err := a.up(ctx, service, true); err != nil {
				logger.Error("failed to start original compose app", zap.Error(err), zap.String("name", a.Name))
				return
			}

		}
	}()

	// save new compose file
	if err := file.WriteToFullPath(newComposeYAML, currentComposeFile, 0o600); err != nil {
		return err
	}

	if newEnv != nil {
		if err := a.WriteEnvFile(*newEnv); err != nil {
			return err
		}
	}

	// every file the app is made of, not only the one just written: an override beside it is
	// part of what docker runs
	newComposeApp, err := LoadComposeAppFromConfigFile(a.Name, strings.Join(a.ComposeFiles, ","))
	if err != nil {
		return err
	}

	if err := newComposeApp.Pull(ctx); err != nil {
		return err
	}

	go PublishEventWrapper(ctx, common.EventTypeContainerStartBegin, nil)

	defer PublishEventWrapper(ctx, common.EventTypeContainerStartEnd, nil)

	// an app that fails to start counts as a failed apply: the files are put back and the
	// previous app started from them, so disk and running state never diverge
	err = newComposeApp.UpWithCheckRequire(ctx, service)
	success = keepNewDefinition(err)
	return err
}

// keepNewDefinition reports whether an apply that ended with err must leave the new
// compose file and `.env` on disk.
//
// A wait that only ran out of time is not a failed apply: compose created and started
// the containers from the new definition and they are running right now. Restoring the
// backup over it and re-upping the old definition would take away an edit that worked
// -- the case that made this deliberate: a stack with a one-shot init container nobody
// depends on can never satisfy compose's running-or-healthy wait, however long it is
// given. The error still travels to the caller, so the owner is told the app was
// started but never confirmed healthy, rather than being told nothing.
func keepNewDefinition(err error) bool {
	return err == nil || errors.Is(err, errUpNotConfirmed)
}

func (a *ComposeApp) Create(ctx context.Context, options api.CreateOptions, service api.Compose) error {
	a.injectEnvVariableToComposeApp()
	return service.Create(ctx, (*codegen.ComposeApp)(a), api.CreateOptions{})
}

func (a *ComposeApp) PullAndInstall(ctx context.Context) error {
	service, dockerClient, err := apiService()
	if err != nil {
		return err
	}
	defer dockerClient.Close()

	// pull
	if err := a.Pull(ctx); err != nil {
		return err
	}

	// create
	if err := func() error {
		go PublishEventWrapper(ctx, common.EventTypeContainerCreateBegin, nil)

		defer PublishEventWrapper(ctx, common.EventTypeContainerCreateEnd, nil)

		for name, app := range a.Services {
			// prepare source path for volumes if not exist
			for _, volume := range app.Volumes {
				if _, ok := a.Volumes[volume.Source]; ok {
					// this is a internal volume, so skip.
					continue
				}

				path := volume.Source
				if err := file.IsNotExistMkDir(path); err != nil {
					go PublishEventWrapper(ctx, common.EventTypeContainerCreateError, map[string]string{
						common.PropertyTypeMessage.Name: err.Error(),
					})
					return err
				}
			}

			// check if each required device exists
			deviceMapFiltered := []types.DeviceMapping{}
			for _, deviceMap := range app.Devices {
				devicePath := deviceMap.Source
				if file.CheckNotExist(devicePath) {
					logger.Info("device not found", zap.String("device", devicePath))
					continue
				}
				deviceMapFiltered = append(deviceMapFiltered, deviceMap)
			}
			app.Devices = deviceMapFiltered
			a.Services[name] = app
		}

		if err := a.Create(ctx, api.CreateOptions{}, service); err != nil {
			go PublishEventWrapper(ctx, common.EventTypeContainerCreateError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})
			return err
		}

		return nil
	}(); err != nil {
		return err
	}

	go PublishEventWrapper(ctx, common.EventTypeContainerStartBegin, nil)

	defer PublishEventWrapper(ctx, common.EventTypeContainerStartEnd, nil)

	if err := waitForApp(ctx, a.Name, func(ctx context.Context) error {
		// With the project, not only its name. Given none, compose rebuilds the project from
		// the containers' labels, which carry no post_start or pre_start hook, so a container
		// CasaOS created never ran its hooks. Every service has its containers here: they were
		// just created.
		return service.Start(ctx, a.Name, api.StartOptions{Project: (*codegen.ComposeApp)(a), Wait: true})
	}, a.everyContainerSettled); err != nil {
		go PublishEventWrapper(ctx, common.EventTypeContainerStartError, map[string]string{
			common.PropertyTypeMessage.Name: err.Error(),
		})
		return err
	}

	return nil
}

// uninstallDownOptions is what an uninstall asks compose to take down. The named volumes are
// the app's data, a database included: they go with the config folder, not before, so that
// leaving the box unticked keeps them for the next install of the app.
func uninstallDownOptions(deleteConfigFolder bool) api.DownOptions {
	return api.DownOptions{
		RemoveOrphans: true,
		Images:        "all",
		Volumes:       deleteConfigFolder,
	}
}

func (a *ComposeApp) Uninstall(ctx context.Context, deleteConfigFolder bool) error {
	service, dockerClient, err := apiService()
	if err != nil {
		return err
	}
	defer dockerClient.Close()

	// stop
	if err := func() error {
		go PublishEventWrapper(ctx, common.EventTypeContainerStopBegin, nil)

		defer PublishEventWrapper(ctx, common.EventTypeContainerStopEnd, nil)

		if err := service.Stop(ctx, a.Name, api.StopOptions{}); err != nil {
			go PublishEventWrapper(ctx, common.EventTypeContainerStopError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})

			return err
		}

		return nil
	}(); err != nil {
		return err
	}

	// remove
	go PublishEventWrapper(ctx, common.EventTypeContainerRemoveBegin, nil)

	defer PublishEventWrapper(ctx, common.EventTypeContainerRemoveEnd, nil)

	if err := service.Down(ctx, a.Name, uninstallDownOptions(deleteConfigFolder)); err != nil {
		go PublishEventWrapper(ctx, common.EventTypeImageRemoveError, map[string]string{
			common.PropertyTypeMessage.Name: err.Error(),
		})

		return err
	}

	a.removeUninstalledFolders(ctx, deleteConfigFolder)

	// a schedule of an app that is gone fails every night; its backups stay where they went
	if err := forgetBackupSchedules(a.Name); err != nil {
		logger.Error("the app is uninstalled but its backup schedules could not be removed", zap.Error(err), zap.String("app", a.Name))
	}

	return nil
}

// removeUninstalledFolders deletes what an uninstall deletes, once the containers are gone,
// and forgets a git app. A git state that cannot be read may be an adopted app's, whose
// folder is its owner's: nothing is deleted then, and the state stays.
func (a *ComposeApp) removeUninstalledFolders(ctx context.Context, deleteConfigFolder bool) {
	gitState, err := loadGitApp(a.Name)
	if err != nil && !errors.Is(err, ErrGitAppNotFound) {
		logger.Error("the git state of an uninstalled app cannot be read, so its folders are kept", zap.Error(err), zap.String("app", a.Name))

		go PublishEventWrapper(ctx, common.EventTypeImageRemoveError, map[string]string{
			common.PropertyTypeMessage.Name: err.Error(),
		})

		return
	}

	for _, path := range a.uninstalledFolders(gitState, deleteConfigFolder) {
		if err := file.RMDir(path); err != nil {
			logger.Error("failed to remove compose app folder", zap.Error(err), zap.String("path", path))

			go PublishEventWrapper(ctx, common.EventTypeImageRemoveError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})
		}
	}

	if gitState != nil {
		forgetUninstalledGitApp(ctx, gitState)
	}
}

// uninstalledFolders is what an uninstall deletes: the app's folder and, with
// deleteConfigFolder, the folders its volumes name after it. A git app's folder is its
// repository: a created app's goes with the config folder only, and an adopted app's is
// its owner's and never goes.
func (a *ComposeApp) uninstalledFolders(gitState *gitApp, deleteConfigFolder bool) []string {
	keep := func(path string) bool {
		switch {
		case gitState == nil:
			return false
		case gitState.Origin == gitOriginAdopted:
			return path == gitState.Dir || strings.HasPrefix(path, gitState.Dir+"/") || strings.HasPrefix(gitState.Dir, path+"/")
		}

		return path == gitState.Dir && !deleteConfigFolder
	}

	folders := []string{}
	if !keep(a.WorkingDir) {
		folders = append(folders, a.WorkingDir)
	}
	if !deleteConfigFolder {
		return folders
	}

	for _, name := range sortedServiceNames(a.Services) {
		for _, volume := range a.Services[name].Volumes {
			if !strings.Contains(volume.Source, a.Name) {
				continue
			}
			if path := filepath.Join(strings.Split(volume.Source, a.Name)[0], a.Name); !keep(path) {
				folders = append(folders, path)
			}
		}
	}

	return lo.Uniq(folders)
}

func (a *ComposeApp) Apply(ctx context.Context, newComposeYAML []byte) error {
	return a.apply(ctx, newComposeYAML, nil)
}

// ApplyEnv replaces the `.env` of the app (empty removes it) and re-creates it from its unchanged
// compose file: a restart would keep the old environment, the apply reloads the project first.
func (a *ComposeApp) ApplyEnv(ctx context.Context, env []byte) error {
	if len(a.ComposeFiles) <= 0 {
		return ErrComposeFileNotFound
	}

	current, err := os.ReadFile(a.ComposeFiles[0])
	if err != nil {
		return err
	}

	return a.apply(ctx, current, &env)
}

func (a *ComposeApp) apply(ctx context.Context, newComposeYAML []byte, newEnv *[]byte) error {
	if composeYAMLNamesAnotherApp(newComposeYAML, a.Name) {
		return ErrComposeAppNotMatch
	}

	// in the app's own folder: parsed in a temporary one, a relative `env_file: .env` was looked
	// for there, and every save of an app that has one failed, its settings and its .env alike
	newComposeApp, err := newComposeAppFromYAML(newComposeYAML, true, true, nil, a.WorkingDir, nil, nil)
	if err != nil {
		return err
	}

	if len(a.ComposeFiles) <= 0 {
		return ErrComposeFileNotFound
	}

	if len(a.ComposeFiles) > 1 {
		logger.Info("the app is made of several compose files: a save rewrites the first one, and every one of them still applies", zap.String("compose files", strings.Join(a.ComposeFiles, ",")))
	}

	// prepare for message bus events
	eventProperties := map[string]string{common.PropertyTypeAppName.Name: a.Name}

	// prepare for message bus events
	if err := newComposeApp.UpdateEventPropertiesFromStoreInfo(eventProperties); err != nil {
		logger.Info("failed to update event properties from store info", zap.Error(err), zap.String("name", a.Name))
	}

	common.SetProperties(ctx, eventProperties)

	end, err := Begin(a.Name, "save")
	if err != nil {
		return err
	}

	go func(ctx context.Context) {
		defer end()

		go PublishEventWrapper(ctx, common.EventTypeAppApplyChangesBegin, nil)

		defer PublishEventWrapper(ctx, common.EventTypeAppApplyChangesEnd, nil)

		if err := a.pullAndApply(ctx, newComposeYAML, newEnv); err != nil {
			go PublishEventWrapper(ctx, common.EventTypeAppApplyChangesError, map[string]string{
				common.PropertyTypeMessage.Name: err.Error(),
			})

			logger.Error("failed to apply changes to compose app", zap.Error(err), zap.String("name", a.Name))
		}
	}(ctx)

	return nil
}

func (a *ComposeApp) SetStatus(ctx context.Context, status codegen.RequestComposeAppStatus) error {
	service, dockerClient, err := apiService()
	if err != nil {
		return err
	}
	defer dockerClient.Close()

	common.SetProperties(ctx, map[string]string{common.PropertyTypeAppName.Name: a.Name})

	switch status {
	case codegen.RequestComposeAppStatusStart:
		go func(ctx context.Context) {
			go PublishEventWrapper(ctx, common.EventTypeAppStartBegin, nil)

			defer PublishEventWrapper(ctx, common.EventTypeAppStartEnd, nil)

			// the user wants the app running again, drop any stale marker
			clearAppStopped(a.Name)

			// to make sure the container is stopped
			// timeout is 20s
			for index := 0; index < 10; index++ {
				containerSummarys, err := service.Ps(ctx, a.Name, api.PsOptions{
					All: true,
				})
				if err != nil {
					logger.Error("failed to get compose app info", zap.Error(err), zap.String("name", a.Name))
				}
				isContainerExited := true
				for _, containerSummary := range containerSummarys {
					// to make sure every service of the container is stopped
					// I think "exited" can be replace by constant value.
					isContainerExited = isContainerExited && (containerSummary.State == "exited")
				}
				if isContainerExited {
					break
				}
				time.Sleep(2 * time.Second)
			}

			if err := waitForApp(ctx, a.Name, func(ctx context.Context) error {
				return service.Start(ctx, a.Name, api.StartOptions{Wait: true})
			}, a.everyContainerSettled); err != nil {
				go PublishEventWrapper(ctx, common.EventTypeAppStartError, map[string]string{
					common.PropertyTypeMessage.Name: err.Error(),
				})

				logger.Error("failed to start compose app", zap.Error(err), zap.String("name", a.Name))
			}
		}(ctx)
	case codegen.RequestComposeAppStatusStop:
		go func(ctx context.Context) {
			go PublishEventWrapper(ctx, common.EventTypeAppStopBegin, nil)

			defer PublishEventWrapper(ctx, common.EventTypeAppStopEnd, nil)

			if err := service.Stop(ctx, a.Name, api.StopOptions{}); err != nil {
				go PublishEventWrapper(ctx, common.EventTypeAppStopError, map[string]string{
					common.PropertyTypeMessage.Name: err.Error(),
				})

				logger.Error("failed to stop compose app", zap.Error(err), zap.String("name", a.Name))
				return
			}

			markAppStopped(a.Name)
		}(ctx)
	case codegen.RequestComposeAppStatusRestart:
		go func(ctx context.Context) {
			go PublishEventWrapper(ctx, common.EventTypeAppRestartBegin, nil)

			defer PublishEventWrapper(ctx, common.EventTypeAppRestartEnd, nil)

			clearAppStopped(a.Name)

			if err := service.Restart(ctx, a.Name, api.RestartOptions{}); err != nil {
				go PublishEventWrapper(ctx, common.EventTypeAppRestartError, map[string]string{
					common.PropertyTypeMessage.Name: err.Error(),
				})

				logger.Error("failed to restart compose app", zap.Error(err), zap.String("name", a.Name))
			}
		}(ctx)
	default:
		return ErrInvalidComposeAppStatus
	}

	return nil
}

// Logs returns the logs of the app. Naming services narrows it to those; naming none
// keeps every service of the app, which is what the app-wide viewer asks for.
func (a *ComposeApp) Logs(ctx context.Context, lines int, services ...string) ([]byte, error) {
	service, dockerClient, err := apiService()
	if err != nil {
		return nil, err
	}
	defer dockerClient.Close()

	var buf bytes.Buffer

	if err := service.Logs(ctx, a.Name, newLogConsumer(ctx, &buf), a.logOptions(lines, services...)); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// logOptions is what Logs asks the daemon for. Timestamps is the point of it: the log
// viewer shows when a line was written, and only the daemon knows that.
//
// Split out from the call so it can be read back without a daemon -- everything below
// is a promise to the viewer that nothing else in this package can check.
func (a *ComposeApp) logOptions(lines int, services ...string) api.LogOptions {
	if len(services) == 0 {
		services = sortedServiceNames(a.Services)
	}

	return api.LogOptions{
		Project:    (*codegen.ComposeApp)(a),
		Services:   services,
		Follow:     false,
		Timestamps: true,
		Tail:       lo.If(lines < 0, "all").Else(strconv.Itoa(lines)),
	}
}

// newLogConsumer formats what comes back: the service name as a prefix, and the line
// otherwise untouched.
//
// The last flag is the consumer's own timestamp, and it stays off. It stamps
// time.Now() at READ time, so a tail of an hour of logs would come back with every
// line claiming this instant -- on top of the daemon's real one, which the options
// above already asked for.
func newLogConsumer(ctx context.Context, w io.Writer) api.LogConsumer {
	return formatter.NewLogConsumer(ctx, w, w, false, true, false)
}

func (a *ComposeApp) GetPortsInUse() (*codegen.ComposeAppValidationErrorsPortsInUse, error) {
	return a.portsInUse(a.Services)
}

// GetPortsInUseFor is GetPortsInUse for what runs with the profiles on: the ports of a service
// behind a profile that is off (a proxy that is not installed here) are no conflict.
func (a *ComposeApp) GetPortsInUseFor(profiles []string) (*codegen.ComposeAppValidationErrorsPortsInUse, error) {
	running := types.Services{}
	for name, s := range a.Services {
		if len(s.Profiles) == 0 || lo.Contains(profiles, "*") || len(lo.Intersect(s.Profiles, profiles)) > 0 {
			running[name] = s
		}
	}

	return a.portsInUse(running)
}

func (a *ComposeApp) portsInUse(services types.Services) (*codegen.ComposeAppValidationErrorsPortsInUse, error) {
	tcpPorts, udpPorts, err := port.ListPortsInUse()
	if err != nil {
		return nil, err
	}

	allPortsInUse := lo.Union(tcpPorts, udpPorts)

	tcpPortInUse := []string{}
	udpPortInUse := []string{}

	for _, s := range services {
		for _, p := range s.Ports {
			if lo.ContainsBy(allPortsInUse, func(portInUse int) bool { return strconv.Itoa(portInUse) == p.Published }) {
				switch strings.ToLower(p.Protocol) {
				case "tcp":
					tcpPortInUse = append(tcpPortInUse, p.Published)
				case "udp":
					udpPortInUse = append(udpPortInUse, p.Published)
				}
			}
		}
	}

	if len(tcpPortInUse) == 0 && len(udpPortInUse) == 0 {
		return nil, nil
	}

	portsInUse := struct {
		TCP *codegen.PortList "json:\"TCP,omitempty\""
		UDP *codegen.PortList "json:\"UDP,omitempty\""
	}{TCP: &tcpPortInUse, UDP: &udpPortInUse}

	return &codegen.ComposeAppValidationErrorsPortsInUse{PortsInUse: &portsInUse}, nil
}

// Try to update AppIcon and AppTitle in given event properties from store info
func (a *ComposeApp) UpdateEventPropertiesFromStoreInfo(eventProperties map[string]string) error {
	if eventProperties == nil {
		return fmt.Errorf("event properties is nil")
	}

	storeInfo, err := a.StoreInfo(false)
	if err != nil {
		return err
	}

	eventProperties[common.PropertyTypeAppIcon.Name] = storeInfo.Icon

	if storeInfo.Title == nil {
		return fmt.Errorf("compose app title not found in store info")
	}

	titles, err := json.Marshal(storeInfo.Title)
	if err != nil {
		return err
	}

	eventProperties[common.PropertyTypeAppTitle.Name] = string(titles)

	return nil
}

func (a *ComposeApp) HealthCheck() (bool, error) {
	storeInfo, err := a.StoreInfo(false)
	if err != nil {
		return false, err
	}

	scheme := "http"
	if storeInfo.Scheme != nil && *storeInfo.Scheme != "" {
		scheme = string(*storeInfo.Scheme)
	}

	hostname := common.Localhost
	if storeInfo.Hostname != nil && *storeInfo.Hostname != "" {
		hostname = *storeInfo.Hostname
	}

	url := fmt.Sprintf(
		"%s://%s:%s/%s",
		scheme,
		hostname,
		storeInfo.PortMap,
		strings.TrimLeft(storeInfo.Index, "/"),
	)

	logger.Info("checking compose app health at the specified web port...", zap.String("name", a.Name), zap.Any("url", url))

	client := resty.New()
	client.SetTimeout(30 * time.Second)
	client.SetHeader("Accept", "text/html")
	// ignore ssl error
	client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: true})
	response, err := client.R().Get(url)
	if err != nil {
		logger.Error("failed to check container health", zap.Error(err), zap.String("name", a.Name))
		return false, err
	}
	if response.StatusCode() == http.StatusOK || response.StatusCode() == http.StatusUnauthorized {
		return true, nil
	}

	logger.Error("compose app health check failed at the specified web port", zap.Any("name", a.Name), zap.Any("url", url), zap.String("status", fmt.Sprint(response.StatusCode())))
	return false, nil
}

func LoadComposeAppFromConfigFile(appID string, configFile string) (*ComposeApp, error) {
	// Every file compose recorded, named explicitly. configFile is one path, or the
	// comma-separated list a stack's containers carry when it was started from several (an
	// override beside the main file, `-f a.yml -f b.yml`). Left to look for a default name in
	// the directory, compose refused a file called anything else and took a comma-joined list
	// for one directory name, and such a stack vanished from the compose list.
	configFiles := []string{}
	for _, f := range strings.Split(configFile, ",") {
		if f = strings.TrimSpace(f); f != "" {
			configFiles = append(configFiles, f)
		}
	}
	if len(configFiles) == 0 {
		return nil, ErrComposeFileNotFound
	}

	options := composeCmd.ProjectOptions{
		ConfigPaths: configFiles,
		ProjectDir:  filepath.Dir(configFiles[0]),
		ProjectName: appID,
	}

	env := []string{fmt.Sprintf("%s=%s", "AppID", appID)}
	for k, v := range baseInterpolationMap() {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	// compose v5 loads through a Compose backend. Loading never reaches the daemon, so the
	// backend gets no Docker CLI, as ToProject got none with v2.
	backend, err := compose.NewComposeService(nil)
	if err != nil {
		return nil, err
	}

	// load project
	project, _, err := options.ToProject(
		context.Background(),
		nil,
		backend,
		nil,
		cli.WithWorkingDirectory(options.ProjectDir), // this has to be the first option, otherwise it will assume the dir where this program is running is the working directory.

		cli.WithOsEnv,
		cli.WithDotEnv,
		cli.WithEnv(env),
		cli.WithConfigFileEnv,
		cli.WithDefaultConfigPath,
		cli.WithEnvFiles(options.EnvFiles...),
		cli.WithName(options.ProjectName),
	)

	return (*ComposeApp)(project), err
}

var gpuCache *([]external.NvidiaGPUInfo) = nil

func removeRuntime(a *ComposeApp) {
	if config.RemoveRuntimeIfNoNvidiaGPUFlag {

		// if gpuCache is nil, it means it is first time fetching gpu info
		if gpuCache == nil {
			value, err := external.NvidiaGPUInfoList()
			if err != nil {
				gpuCache = &([]external.NvidiaGPUInfo{})
			} else {
				gpuCache = &value
			}

			// without nvidia-smi 	// no gpu or first time fetching gpu info failed
		}
		if len(*gpuCache) == 0 {
			for name, service := range a.Services {
				service.Runtime = ""
				a.Services[name] = service
			}
		}
	}
}

func NewComposeAppFromYAML(yaml []byte, skipInterpolation, skipValidation bool) (*ComposeApp, error) {
	return newComposeAppFromYAML(yaml, skipInterpolation, skipValidation, nil, "", nil, nil)
}

// keep is non-nil for the settings pipeline (ComposeAppFromSettingsYAML): the output is compose
// text again, see substituteEscaping. nil resolves everything as before.
// values are what a .env installed with the text defines (NewComposeAppWithEnv): they resolve
// before the environment of this process. profiles are the profiles turned on: a service of
// another profile is left out, as docker compose leaves it.
func newComposeAppFromYAML(yaml []byte, skipInterpolation, skipValidation bool, keep map[string]struct{}, workingDir string, values map[string]string, profiles []string) (*ComposeApp, error) {
	// workingDir is where the relative paths of the text resolve (env_file, a build context, a
	// ./ volume): the app's own folder for an installed app's file, a temporary folder for text
	// that has no folder yet.
	if workingDir == "" {
		tmpWorkingDir, err := os.MkdirTemp("", "casaos-compose-app-*")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(tmpWorkingDir)
		workingDir = tmpWorkingDir
	}

	// the WEBUI_PORT interpolate will tiger twice. In `pulished` and `port-map`.
	// So we need to promise multiple WEBUI_PORT interpolate is a same value.
	port, _ := port.GetAvailablePort("tcp")

	// a quoted number where the specification wants an integer is a number to
	// everyone but compose-go's schema; see compose_numeric_strings.go
	yaml = coerceNumericStrings(yaml)
	fallbacks := variablesWithFallback(yaml)

	project, err := loader.LoadWithContext(context.Background(),
		types.ConfigDetails{
			ConfigFiles: []types.ConfigFile{
				{
					Content: []byte(yaml),
				},
			},
			Environment: keepRefs(keep),

			// need to set a working dir because loader/normalize.go from github.com/compose-spec/compose-go makes
			// wrong assumption that the working dir is the same as the dir where this program is launched.
			WorkingDir: workingDir,
		},
		func(o *loader.Options) {
			o.SkipInterpolation = skipInterpolation
			o.SkipValidation = skipValidation
			o.Profiles = profiles
			if keep != nil {
				keepPathRefs(o)
				keepRefCasts(o)
				o.Interpolate.Substitute = substituteEscaping(settingsKeep(keep))
			}

			o.Interpolate.LookupValue = func(key string) (string, bool) {
				switch key {
				case "WEBUI_PORT":
					fmt.Printf("WEBUI_PORT is not specified, using %d\n", port)
					return strconv.Itoa(port), true
				}

				for k := range baseInterpolationMap() {
					if k == key {
						// example:  TZ => $TZ
						// we didn't want to interpolate base interpolation value.
						// they should be interpolated in LoadComposeAppFromConfig
						return fmt.Sprintf("$%s", k), true
					}
				}
				if value, ok := values[key]; ok {
					return value, true
				}
				// a service started by systemd has no HOME, where docker compose gives ${HOME} the
				// user's home: without it ${HOME}/.docker/config.json is the name of a volume
				if key == "HOME" {
					if dir, err := userHome(); err == nil {
						return dir, true
					}
				}
				// the function may can to replace the above code.
				value, ok := os.LookupEnv(key)
				if ok {
					return value, true
				}
				// ${KEY:-default} and ${KEY:?message} mean something when KEY has no value: say so, and the
				// default (or the message) applies. Any other unknown KEY stays $KEY, for the app's settings,
				// and a key the settings pipeline keeps (a key of the app's .env) stays as it is written.
				if _, kept := keep[key]; fallbacks[key] && !kept {
					return "", false
				}
				return fmt.Sprintf("$%s", key), true
			}

			if getNameFrom(yaml) != "" {
				return
			}

			// fix compose app name
			logger.Info("compose app name is not specified, getting a name from one of our contributors :)")
			projectName := random.Name(nil)
			logger.Info("compose app name is given", zap.String("name", projectName))
			o.SetProjectName(projectName, false)
		},
	)
	if err != nil {
		return nil, err
	}

	composeApp := (*ComposeApp)(project)

	if composeApp.Extensions == nil {
		composeApp.Extensions = map[string]interface{}{}
	}

	storeInfo, err := composeApp.StoreInfo(false)

	if err != nil || storeInfo == nil || storeInfo.Title == nil {
		logger.Info("compose app does not have store info with title set, re-using app name as title", zap.String("app", composeApp.Name))
		composeApp.SetTitle(composeApp.Name, common.DefaultLanguage)
	}

	removeRuntime(composeApp)

	// pass icon information to v1 label for backward compatibility, because we are
	// still using `func getContainerStats()` from `container.go` to get container stats
	// (we are being lazy to upgrade that v1 API to v2 - please help if you can :D)
	if err == nil && storeInfo != nil && storeInfo.Icon != "" {
		for name, service := range composeApp.Services {
			if service.Labels == nil {
				service.Labels = map[string]string{}
			}
			service.Labels[v1.V1LabelIcon] = storeInfo.Icon
			composeApp.Services[name] = service
		}
	}

	return composeApp, nil
}

// composeYAMLNamesAnotherApp says whether compose text declares a project name other than
// name. Text that declares none is named by its stack, as compose does: a stack started by
// hand rarely has `name:`, and the .env save re-applies the app's own file, which used to be
// refused as another app's.
func composeYAMLNamesAnotherApp(composeYAML []byte, name string) bool {
	declared := getNameFrom(composeYAML)
	return declared != "" && declared != name
}

func getNameFrom(composeYAML []byte) string {
	var baseStructure struct {
		Name string `yaml:"name"`
	}

	if err := yaml.Unmarshal(composeYAML, &baseStructure); err != nil {
		return ""
	}

	return baseStructure.Name
}

func (a *ComposeApp) SetUncontrolled(uncontrolled bool) error {
	xCasaos := a.Extensions[common.ComposeExtensionNameXCasaOS]
	xCasaosMap, ok := xCasaos.(map[string]interface{})

	// set to controlled app
	if !ok {
		logger.Error("failed to get map compose app extensions", zap.String("composeAppID", a.Name))
		return ErrComposeExtensionNameXCasaOSNotFound
	} else {
		xCasaosMap[common.ComposeExtensionPropertyNameIsUncontrolled] = uncontrolled
		a.Extensions[common.ComposeExtensionNameXCasaOS] = xCasaosMap
	}

	return nil
}
