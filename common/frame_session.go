/*
 *
 * xk6-browser - a browser automation extension for k6
 * Copyright (C) 2021 Load Impact
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as
 * published by the Free Software Foundation, either version 3 of the
 * License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package common

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/inspector"
	"github.com/chromedp/cdproto/log"
	"github.com/chromedp/cdproto/network"
	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/security"
	"github.com/chromedp/cdproto/target"
	"github.com/grafana/xk6-browser/api"
	"github.com/sirupsen/logrus"
	k6lib "go.k6.io/k6/lib"
	k6stats "go.k6.io/k6/stats"
)

const utilityWorldName = "__k6_browser_utility_world__"

/*
   FrameSession is used for managing a frame's life-cycle, or in other words its full session.
   It manages all the event listening while deferring the state storage to the Frame and FrameManager
   structs.
*/
type FrameSession struct {
	ctx            context.Context
	session        *Session
	page           *Page
	parent         *FrameSession
	manager        *FrameManager
	networkManager *NetworkManager

	targetID target.ID
	windowID browser.WindowID

	initTime *cdp.MonotonicTime

	// To understand the concepts of Isolated Worlds, Contexts and Frames and
	// the relationship betwween them have a look at the following doc:
	// https://chromium.googlesource.com/chromium/src/+/master/third_party/blink/renderer/bindings/core/v8/V8BindingDesign.md
	contextIDToContextMu sync.Mutex
	contextIDToContext   map[runtime.ExecutionContextID]*ExecutionContext
	isolatedWorlds       map[string]bool

	eventCh chan Event

	childSessions map[cdp.FrameID]*FrameSession

	logger *Logger
	// logger that will properly serialize RemoteObject instances
	serializer *logrus.Logger
}

func NewFrameSession(
	ctx context.Context, session *Session, page *Page, parent *FrameSession,
	targetID target.ID, logger *Logger,
) (*FrameSession, error) {
	logger.Debugf("NewFrameSession", "sid:%v tid:%v", session.id, targetID)

	fs := FrameSession{
		ctx:                  ctx, // TODO: create cancelable context that can be used to cancel and close all child sessions
		session:              session,
		page:                 page,
		parent:               parent,
		manager:              page.frameManager,
		networkManager:       nil,
		targetID:             targetID,
		initTime:             &cdp.MonotonicTime{},
		contextIDToContextMu: sync.Mutex{},
		contextIDToContext:   make(map[runtime.ExecutionContextID]*ExecutionContext),
		isolatedWorlds:       make(map[string]bool),
		eventCh:              make(chan Event),
		childSessions:        make(map[cdp.FrameID]*FrameSession),
		logger:               logger,
		serializer: &logrus.Logger{
			Out:       logger.log.Out,
			Level:     logger.log.Level,
			Formatter: &consoleLogFormatter{logger.log.Formatter},
		},
	}
	var err error
	if fs.parent != nil {
		fs.networkManager, err = NewNetworkManager(ctx, session, fs.manager, fs.parent.networkManager)
		if err != nil {
			logger.Debugf("NewFrameSession:NewNetworkManager", "sid:%v tid:%v err:%v", session.id, targetID, err)
			return nil, err
		}
	} else {
		fs.networkManager, err = NewNetworkManager(ctx, session, fs.manager, nil)
		if err != nil {
			logger.Debugf("NewFrameSession:NewNetworkManager#2", "sid:%v tid:%v err:%v", session.id, targetID, err)
			return nil, err
		}
	}

	action := browser.GetWindowForTarget().WithTargetID(fs.targetID)
	if fs.windowID, _, err = action.Do(cdp.WithExecutor(fs.ctx, fs.session)); err != nil {
		logger.Debugf("NewFrameSession:GetWindowForTarget", "sid:%v tid:%v err:%v", session.id, targetID, err)
		return nil, fmt.Errorf(`unable to get window ID: %w`, err)
	}

	fs.initEvents()
	if err = fs.initFrameTree(); err != nil {
		logger.Debugf("NewFrameSession:initFrameTree", "sid:%v tid:%v err:%v", session.id, targetID, err)
		return nil, err
	}
	if err = fs.initIsolatedWorld(utilityWorldName); err != nil {
		logger.Debugf("NewFrameSession:initIsolatedWorld", "sid:%v tid:%v err:%v", session.id, targetID, err)
		return nil, err
	}
	if err = fs.initDomains(); err != nil {
		logger.Debugf("NewFrameSession:initDomains", "sid:%v tid:%v err:%v", session.id, targetID, err)
		return nil, err
	}
	if err = fs.initOptions(); err != nil {
		logger.Debugf("NewFrameSession:initOptions", "sid:%v tid:%v err:%v", session.id, targetID, err)
		return nil, err
	}
	logger.Debugf("NewFrameSession", "sid:%v tid:%v wid:%v err:%v", session.id, targetID, fs.windowID, err)
	return &fs, nil
}

func (fs *FrameSession) emulateLocale() error {
	action := emulation.SetLocaleOverride().WithLocale(fs.page.browserCtx.opts.Locale)
	if err := action.Do(cdp.WithExecutor(fs.ctx, fs.session)); err != nil {
		if strings.Contains(err.Error(), "Another locale override is already in effect") {
			return nil
		}
		return fmt.Errorf(`unable to set locale: %w`, err)
	}
	return nil
}

func (fs *FrameSession) emulateTimezone() error {
	action := emulation.SetTimezoneOverride(fs.page.browserCtx.opts.TimezoneID)
	if err := action.Do(cdp.WithExecutor(fs.ctx, fs.session)); err != nil {
		if strings.Contains(err.Error(), "Timezone override is already in effect") {
			return nil
		}
		return fmt.Errorf(`unable to set timezone ID: %w`, err)
	}
	return nil
}

func (fs *FrameSession) getNetworkManager() *NetworkManager {
	return fs.networkManager
}

func (fs *FrameSession) initDomains() error {
	actions := []Action{
		dom.Enable(), // TODO: can we get rid of this by doing DOM related stuff in JS instead?
		log.Enable(),
		runtime.Enable(),
		target.SetAutoAttach(true, true).WithFlatten(true),
	}
	for _, action := range actions {
		if err := action.Do(cdp.WithExecutor(fs.ctx, fs.session)); err != nil {
			return fmt.Errorf("unable to execute %T: %w", action, err)
		}
	}
	return nil
}

func (fs *FrameSession) initEvents() {
	fs.logger.Debugf("NewFrameSession:initEvents",
		"sid:%v tid:%v", fs.session.id, fs.targetID)

	events := []string{
		cdproto.EventInspectorTargetCrashed,
	}
	fs.session.on(fs.ctx, events, fs.eventCh)
	if !fs.isMainFrame() {
		fs.initRendererEvents()
	}

	go func() {
		fs.logger.Debugf("NewFrameSession:initEvents:go", "sid:%v tid:%v", fs.session.id, fs.targetID)
		defer fs.logger.Debugf("NewFrameSession:initEvents:go:return", "sid:%v tid:%v", fs.session.id, fs.targetID)

		for {
			select {
			case <-fs.ctx.Done():
				fs.logger.Debugf(
					"NewFrameSession:initEvents:go:ctx.Done",
					"sid:%v tid:%v",
					fs.session.id, fs.targetID)
				return
			case event := <-fs.eventCh:
				if ev, ok := event.data.(*inspector.EventTargetCrashed); ok {
					fs.onTargetCrashed(ev)
				} else if ev, ok := event.data.(*log.EventEntryAdded); ok {
					fs.onLogEntryAdded(ev)
				} else if ev, ok := event.data.(*cdppage.EventFrameAttached); ok {
					fs.onFrameAttached(ev.FrameID, ev.ParentFrameID)
				} else if ev, ok := event.data.(*cdppage.EventFrameDetached); ok {
					fs.onFrameDetached(ev.FrameID)
				} else if ev, ok := event.data.(*cdppage.EventFrameNavigated); ok {
					fs.onFrameNavigated(ev.Frame, false)
				} else if ev, ok := event.data.(*cdppage.EventFrameRequestedNavigation); ok {
					fs.onFrameRequestedNavigation(ev)
				} else if ev, ok := event.data.(*cdppage.EventFrameStartedLoading); ok {
					fs.onFrameStartedLoading(ev.FrameID)
				} else if ev, ok := event.data.(*cdppage.EventFrameStoppedLoading); ok {
					fs.onFrameStoppedLoading(ev.FrameID)
				} else if ev, ok := event.data.(*cdppage.EventLifecycleEvent); ok {
					fs.onPageLifecycle(ev)
				} else if ev, ok := event.data.(*cdppage.EventNavigatedWithinDocument); ok {
					fs.onPageNavigatedWithinDocument(ev)
				} else if ev, ok := event.data.(*runtime.EventConsoleAPICalled); ok {
					fs.onConsoleAPICalled(ev)
				} else if ev, ok := event.data.(*runtime.EventExceptionThrown); ok {
					fs.onExceptionThrown(ev)
				} else if ev, ok := event.data.(*runtime.EventExecutionContextCreated); ok {
					fs.onExecutionContextCreated(ev)
				} else if ev, ok := event.data.(*runtime.EventExecutionContextDestroyed); ok {
					fs.onExecutionContextDestroyed(ev.ExecutionContextID)
				} else if _, ok := event.data.(*runtime.EventExecutionContextsCleared); ok {
					fs.onExecutionContextsCleared()
				} else if ev, ok := event.data.(*target.EventAttachedToTarget); ok {
					fs.onAttachedToTarget(ev)
				} else if ev, ok := event.data.(*target.EventDetachedFromTarget); ok {
					fs.onDetachedFromTarget(ev)
				}
			}
		}
	}()
}

func (fs *FrameSession) initFrameTree() error {
	fs.logger.Debugf("NewFrameSession:initFrameTree",
		"sid:%v tid:%v", fs.session.id, fs.targetID)

	action := cdppage.Enable()
	if err := action.Do(cdp.WithExecutor(fs.ctx, fs.session)); err != nil {
		return fmt.Errorf("unable to enable page domain: %w", err)
	}

	var frameTree *cdppage.FrameTree
	var err error

	// Recursively enumerate all existing frames in page to create initial in-memory structures
	// used for access and manipulation from JS.
	action2 := cdppage.GetFrameTree()
	if frameTree, err = action2.Do(cdp.WithExecutor(fs.ctx, fs.session)); err != nil {
		return fmt.Errorf("unable to get page frame tree: %w", err)
	}

	// FIXME: frameTree is sometimes nil on Windows
	// https://github.com/grafana/xk6-browser/runs/4036384507

	if fs.isMainFrame() {
		fs.handleFrameTree(frameTree)
		fs.initRendererEvents()
	}
	return nil
}

func (fs *FrameSession) initIsolatedWorld(name string) error {
	fs.logger.Debugf("NewFrameSession:initIsolatedWorld",
		"sid:%v tid:%v", fs.session.id, fs.targetID)

	action := cdppage.SetLifecycleEventsEnabled(true)
	if err := action.Do(cdp.WithExecutor(fs.ctx, fs.session)); err != nil {
		return fmt.Errorf(`unable to enable page lifecycle events: %w`, err)
	}

	if _, ok := fs.isolatedWorlds[name]; ok {
		fs.logger.Debugf("NewFrameSession:initIsolatedWorld",
			"sid:%v tid:%v, not found: %q", fs.session.id, fs.targetID, name)
		return nil
	}
	fs.isolatedWorlds[name] = true

	var frames []api.Frame
	if fs.isMainFrame() {
		frames = fs.manager.Frames()
	} else {
		frames = []api.Frame{fs.manager.getFrameByID(cdp.FrameID(fs.targetID))}
	}
	for _, frame := range frames {
		// A frame could have been removed before we execute this, so don't wait around for a reply.
		fs.session.ExecuteWithoutExpectationOnReply(fs.ctx, cdppage.CommandCreateIsolatedWorld, &cdppage.CreateIsolatedWorldParams{
			FrameID:             cdp.FrameID(frame.ID()),
			WorldName:           name,
			GrantUniveralAccess: true,
		}, nil)
	}

	fs.logger.Debugf(
		"NewFrameSession:initIsolatedWorld:AddScriptToEvaluateOnNewDocument",
		"sid:%v tid:%v", fs.session.id, fs.targetID)

	action2 := cdppage.AddScriptToEvaluateOnNewDocument(`//# sourceURL=` + evaluationScriptURL).
		WithWorldName(name)
	if _, err := action2.Do(cdp.WithExecutor(fs.ctx, fs.session)); err != nil {
		// TODO: find out whether select is necessary here
		// select {
		// case <-fs.ctx.Done():
		// 	return nil
		// default:
		return fmt.Errorf("unable to add script to evaluate for isolated world: %w", err)
		// }
	}
	return nil
}

func (fs *FrameSession) initOptions() error {
	fs.logger.Debugf("NewFrameSession:initOptions",
		"sid:%v tid:%v", fs.session.id, fs.targetID)

	opts := fs.manager.page.browserCtx.opts
	optActions := []Action{}

	if fs.isMainFrame() {
		optActions = append(optActions, emulation.SetFocusEmulationEnabled(true))
		if err := fs.updateViewport(); err != nil {
			fs.logger.Debugf(
				"NewFrameSession:initOptions:updateViewport",
				"sid:%v tid:%v, err:%v",
				fs.session.id, fs.targetID, err)
			return err
		}
	}
	if opts.BypassCSP {
		optActions = append(optActions, cdppage.SetBypassCSP(true))
	}
	if opts.IgnoreHTTPSErrors {
		optActions = append(optActions, security.SetIgnoreCertificateErrors(true))
	}
	if opts.HasTouch {
		optActions = append(optActions, emulation.SetTouchEmulationEnabled(true))
	}
	if !opts.JavaScriptEnabled {
		optActions = append(optActions, emulation.SetScriptExecutionDisabled(true))
	}
	if opts.UserAgent != "" || opts.Locale != "" {
		optActions = append(optActions, emulation.SetUserAgentOverride(opts.UserAgent).WithAcceptLanguage(opts.Locale))
	}
	if opts.Locale != "" {
		if err := fs.emulateLocale(); err != nil {
			return err
		}
	}
	if opts.TimezoneID != "" {
		if err := fs.emulateTimezone(); err != nil {
			return err
		}
	}
	if err := fs.updateGeolocation(true); err != nil {
		return err
	}
	fs.updateExtraHTTPHeaders(true)
	if err := fs.updateRequestInterception(true); err != nil {
		return err
	}
	fs.updateOffline(true)
	fs.updateHttpCredentials(true)
	if err := fs.updateEmulateMedia(true); err != nil {
		return err
	}

	// if (screencastOptions)
	//   promises.push(this._startVideoRecording(screencastOptions));

	/*for (const source of this._crPage._browserContext._evaluateOnNewDocumentSources)
	      promises.push(this._evaluateOnNewDocument(source, 'main'));
	  for (const source of this._crPage._page._evaluateOnNewDocumentSources)
	      promises.push(this._evaluateOnNewDocument(source, 'main'));*/

	optActions = append(optActions, runtime.RunIfWaitingForDebugger())

	for _, action := range optActions {
		if err := action.Do(cdp.WithExecutor(fs.ctx, fs.session)); err != nil {
			return fmt.Errorf("unable to execute %T: %w", action, err)
		}
	}

	return nil
}

func (fs *FrameSession) initRendererEvents() {
	fs.logger.Debugf("NewFrameSession:initEvents:initRendererEvents",
		"sid:%v tid:%v", fs.session.id, fs.targetID)

	events := []string{
		cdproto.EventLogEntryAdded,
		cdproto.EventPageFileChooserOpened,
		cdproto.EventPageFrameAttached,
		cdproto.EventPageFrameDetached,
		cdproto.EventPageFrameNavigated,
		cdproto.EventPageFrameRequestedNavigation,
		cdproto.EventPageFrameStartedLoading,
		cdproto.EventPageFrameStoppedLoading,
		cdproto.EventPageJavascriptDialogOpening,
		cdproto.EventPageLifecycleEvent,
		cdproto.EventPageNavigatedWithinDocument,
		cdproto.EventRuntimeConsoleAPICalled,
		cdproto.EventRuntimeExceptionThrown,
		cdproto.EventRuntimeExecutionContextCreated,
		cdproto.EventRuntimeExecutionContextDestroyed,
		cdproto.EventRuntimeExecutionContextsCleared,
		cdproto.EventTargetAttachedToTarget,
		cdproto.EventTargetDetachedFromTarget,
	}
	fs.session.on(fs.ctx, events, fs.eventCh)
}

func (fs *FrameSession) isMainFrame() bool {
	return fs.targetID == fs.page.targetID
}

func (fs *FrameSession) handleFrameTree(frameTree *cdppage.FrameTree) {
	fs.logger.Debugf("NewFrameSession:handleFrameTree",
		"sid:%v tid:%v", fs.session.id, fs.targetID)

	if frameTree.Frame.ParentID != "" {
		fs.onFrameAttached(frameTree.Frame.ID, frameTree.Frame.ParentID)
	}
	fs.onFrameNavigated(frameTree.Frame, true)
	if frameTree.ChildFrames == nil {
		return
	}
	for _, child := range frameTree.ChildFrames {
		fs.handleFrameTree(child)
	}
}

func (fs *FrameSession) navigateFrame(frame *Frame, url, referrer string) (string, error) {
	fs.logger.Debugf("NewFrameSession:navigateFrame",
		"sid:%v tid:%v url:%q referrer:%q",
		fs.session.id, fs.targetID, url, referrer)

	action := cdppage.Navigate(url).WithReferrer(referrer).WithFrameID(cdp.FrameID(frame.ID()))
	_, documentID, errorText, err := action.Do(cdp.WithExecutor(fs.ctx, fs.session))
	if err != nil {
		err = fmt.Errorf("%s at %q: %w", errorText, url, err)
	}
	return documentID.String(), err
}

func (fs *FrameSession) onConsoleAPICalled(event *runtime.EventConsoleAPICalled) {
	l := fs.serializer.
		WithTime(event.Timestamp.Time()).
		WithField("source", "browser-console-api")

	if s := k6lib.GetState(fs.ctx); s.Group.Path != "" {
		l = l.WithField("group", s.Group.Path)
	}

	var parsedObjects []interface{}
	for _, robj := range event.Args {
		i, err := parseRemoteObject(robj)
		if err != nil {
			handleParseRemoteObjectErr(fs.ctx, err, l)
		}
		parsedObjects = append(parsedObjects, i)
	}

	l = l.WithField("objects", parsedObjects)

	switch event.Type {
	case "log", "info":
		l.Info()
	case "warning":
		l.Warn()
	case "error":
		l.Error()
	default:
		l.Debug()
	}
}

func (fs *FrameSession) onExceptionThrown(event *runtime.EventExceptionThrown) {
	fs.page.emit(EventPageError, event.ExceptionDetails)
}

func (fs *FrameSession) onExecutionContextCreated(event *runtime.EventExecutionContextCreated) {
	fs.logger.Debugf("NewFrameSession:onExecutionContextCreated",
		"sid:%v tid:%v ecid:%d", fs.session.id, fs.targetID, event.Context.ID)

	auxData := event.Context.AuxData
	var i struct {
		FrameID   cdp.FrameID `json:"frameId"`
		IsDefault bool        `json:"isDefault"`
		Type      string      `json:"type"`
	}
	if err := json.Unmarshal(auxData, &i); err != nil {
		k6Throw(fs.ctx, "unable to unmarshal JSON: %w", err)
	}
	var world string = ""
	frame := fs.manager.getFrameByID(i.FrameID)
	if frame != nil {
		if i.IsDefault {
			world = mainExecutionContext
		} else if event.Context.Name == utilityWorldName && !frame.hasContext(utilityExecutionContext) {
			// In case of multiple sessions to the same target, there's a race between
			// connections so we might end up creating multiple isolated worlds.
			// We can use either.
			world = utilityExecutionContext
		}
	}
	if i.Type == "isolated" {
		fs.isolatedWorlds[event.Context.Name] = true
	}
	context := NewExecutionContext(fs.ctx, fs.session, frame, event.Context.ID, fs.logger)
	if world != "" {
		frame.setContext(world, context)
	}
	fs.contextIDToContextMu.Lock()
	fs.contextIDToContext[event.Context.ID] = context
	fs.contextIDToContextMu.Unlock()
}

func (fs *FrameSession) onExecutionContextDestroyed(execCtxID runtime.ExecutionContextID) {
	fs.logger.Debugf("NewFrameSession:onExecutionContextDestroyed",
		"sid:%v tid:%v ecid:%d", fs.session.id, fs.targetID, execCtxID)

	fs.contextIDToContextMu.Lock()
	defer fs.contextIDToContextMu.Unlock()
	context, ok := fs.contextIDToContext[execCtxID]
	if !ok {
		return
	}
	if context.Frame() != nil {
		context.Frame().nullContext(execCtxID)
	}
	delete(fs.contextIDToContext, execCtxID)
}

func (fs *FrameSession) onExecutionContextsCleared() {
	fs.logger.Debugf("NewFrameSession:onExecutionContextsCleared",
		"sid:%v tid:%v", fs.session.id, fs.targetID)

	fs.contextIDToContextMu.Lock()
	defer fs.contextIDToContextMu.Unlock()

	for _, context := range fs.contextIDToContext {
		if context.Frame() != nil {
			context.Frame().nullContext(context.id)
		}
	}
	for k := range fs.contextIDToContext {
		delete(fs.contextIDToContext, k)
	}
}

func (fs *FrameSession) onFrameAttached(frameID cdp.FrameID, parentFrameID cdp.FrameID) {
	fs.logger.Debugf("NewFrameSession:onFrameAttached",
		"sid:%v tid:%v fid:%v ptid:%v",
		fs.session.id, fs.targetID, frameID, parentFrameID)

	// TODO: add handling for cross-process frame transitioning
	fs.manager.frameAttached(frameID, parentFrameID)
}

func (fs *FrameSession) onFrameDetached(frameID cdp.FrameID) {
	fs.logger.Debugf("NewFrameSession:onFrameDetached",
		"sid:%v tid:%v fid:%v", fs.session.id, fs.targetID, frameID)

	fs.manager.frameDetached(frameID)
}

func (fs *FrameSession) onFrameNavigated(frame *cdp.Frame, initial bool) {
	fs.logger.Debugf("NewFrameSession:onFrameNavigated",
		"sid:%v tid:%v fid:%v", fs.session.id, fs.targetID, frame.ID)

	err := fs.manager.frameNavigated(frame.ID, frame.ParentID, frame.LoaderID.String(), frame.Name, frame.URL+frame.URLFragment, initial)
	if err != nil {
		k6Throw(fs.ctx, "cannot handle frame navigation: %w", err)
	}
}

func (fs *FrameSession) onFrameRequestedNavigation(event *cdppage.EventFrameRequestedNavigation) {
	fs.logger.Debugf("NewFrameSession:onFrameRequestedNavigation",
		"sid:%v tid:%v fid:%v url:%q",
		fs.session.id, fs.targetID, event.FrameID, event.URL)

	if event.Disposition == "currentTab" {
		err := fs.manager.frameRequestedNavigation(event.FrameID, event.URL, "")
		if err != nil {
			k6Throw(fs.ctx, "cannot handle frame requested navigation: %w", err)
		}
	}
}

func (fs *FrameSession) onFrameStartedLoading(frameID cdp.FrameID) {
	fs.logger.Debugf("NewFrameSession:onFrameStartedLoading",
		"sid:%v tid:%v fid:%v", fs.session.id, fs.targetID, frameID)

	fs.manager.frameLoadingStarted(frameID)
}

func (fs *FrameSession) onFrameStoppedLoading(frameID cdp.FrameID) {
	fs.logger.Debugf("NewFrameSession:onFrameStoppedLoading",
		"sid:%v tid:%v fid:%v", fs.session.id, fs.targetID, frameID)

	fs.manager.frameLoadingStopped(frameID)
}

func (fs *FrameSession) onLogEntryAdded(event *log.EventEntryAdded) {
	l := fs.logger.log.
		WithTime(event.Entry.Timestamp.Time()).
		WithField("source", "browser").
		WithField("url", event.Entry.URL).
		WithField("browser_source", event.Entry.Source.String()).
		WithField("line_number", event.Entry.LineNumber)
	if s := k6lib.GetState(fs.ctx); s.Group.Path != "" {
		l = l.WithField("group", s.Group.Path)
	}
	switch event.Entry.Level {
	case "info":
		l.Info(event.Entry.Text)
	case "warning":
		l.Warn(event.Entry.Text)
	case "error":
		l.WithField("stacktrace", event.Entry.StackTrace).Error(event.Entry.Text)
	default:
		l.Debug(event.Entry.Text)
	}
}

func (fs *FrameSession) onPageLifecycle(event *cdppage.EventLifecycleEvent) {
	fs.logger.Debugf("NewFrameSession:onPageLifecycle",
		"sid:%v tid:%v fid:%v event:%q",
		fs.session.id, fs.targetID, event.FrameID, event.Name)

	state := k6lib.GetState(fs.ctx)
	if event.Name == "init" || event.Name == "commit" {
		fs.initTime = event.Timestamp
	}
	if event.Name == "load" {
		fs.manager.frameLifecycleEvent(event.FrameID, LifecycleEventLoad)
		frame := fs.manager.getFrameByID(event.FrameID)
		if frame != nil {
			endTime := event.Timestamp.Time()
			tags := state.CloneTags()
			if state.Options.SystemTags.Has(k6stats.TagURL) {
				tags["url"] = frame.URL()
			}
			sampleTags := k6stats.IntoSampleTags(&tags)
			k6stats.PushIfNotDone(fs.ctx, state.Samples, k6stats.ConnectedSamples{
				Samples: []k6stats.Sample{
					{
						Metric: BrowserLoaded,
						Tags:   sampleTags,
						Value:  k6stats.D(endTime.Sub(fs.initTime.Time())),
						Time:   time.Now(),
					},
				},
			})
		}
	} else if event.Name == "DOMContentLoaded" {
		fs.manager.frameLifecycleEvent(event.FrameID, LifecycleEventDOMContentLoad)
		frame := fs.manager.getFrameByID(event.FrameID)
		if frame != nil {
			endTime := event.Timestamp.Time()
			tags := state.CloneTags()
			if state.Options.SystemTags.Has(k6stats.TagURL) {
				tags["url"] = frame.URL()
			}
			sampleTags := k6stats.IntoSampleTags(&tags)
			k6stats.PushIfNotDone(fs.ctx, state.Samples, k6stats.ConnectedSamples{
				Samples: []k6stats.Sample{
					{
						Metric: BrowserDOMContentLoaded,
						Tags:   sampleTags,
						Value:  k6stats.D(endTime.Sub(fs.initTime.Time())),
						Time:   time.Now(),
					},
				},
			})
		}
	} else {
		eventToMetric := map[string]*k6stats.Metric{
			"firstPaint":           BrowserFirstPaint,
			"firstContentfulPaint": BrowserFirstContentfulPaint,
			"firstMeaningfulPaint": BrowserFirstMeaningfulPaint,
		}
		frame := fs.manager.getFrameByID(event.FrameID)
		if frame != nil {
			if metric, ok := eventToMetric[event.Name]; ok {
				endTime := event.Timestamp.Time()
				tags := state.CloneTags()
				if state.Options.SystemTags.Has(k6stats.TagURL) {
					tags["url"] = frame.URL()
				}
				sampleTags := k6stats.IntoSampleTags(&tags)
				k6stats.PushIfNotDone(fs.ctx, state.Samples, k6stats.ConnectedSamples{
					Samples: []k6stats.Sample{
						{
							Metric: metric,
							Tags:   sampleTags,
							Value:  k6stats.D(endTime.Sub(fs.initTime.Time())),
							Time:   time.Now(),
						},
					},
				})
			}
		}
	}
}

func (fs *FrameSession) onPageNavigatedWithinDocument(event *cdppage.EventNavigatedWithinDocument) {
	fs.logger.Debugf("NewFrameSession:onPageNavigatedWithinDocument",
		"sid:%v tid:%v fid:%v", fs.session.id, fs.targetID, event.FrameID)

	fs.manager.frameNavigatedWithinDocument(event.FrameID, event.URL)
}

func (fs *FrameSession) onAttachedToTarget(event *target.EventAttachedToTarget) {
	fs.logger.Debugf("NewFrameSession:onAttachedToTarget",
		"sid:%v tid:%v esid:%v etid:%v ebid:%v type:%q",
		fs.session.id, fs.targetID, event.SessionID,
		event.TargetInfo.TargetID, event.TargetInfo.BrowserContextID,
		event.TargetInfo.Type)

	session := fs.page.browserCtx.conn.getSession(event.SessionID)
	targetID := event.TargetInfo.TargetID

	if eti := event.TargetInfo; eti.Type == "iframe" && eti.URL != "" {
		frame := fs.manager.getFrameByID(cdp.FrameID(targetID))
		if frame == nil {
			return
		}
		fs.manager.removeChildFramesRecursively(frame)
		frameSession, err := NewFrameSession(fs.ctx, session, fs.page, fs, targetID, fs.logger)
		if err != nil {
			if !fs.page.browserCtx.browser.connected && strings.Contains(err.Error(), "websocket: close 1006 (abnormal closure)") {
				// If we're no longer connected to browser, then ignore WebSocket errors
				return
			}
			select {
			case <-fs.ctx.Done():
				fs.logger.Debugf("FrameSession:onAttachedToTarget:NewFrameSession:<-ctx.Done",
					"sid:%v tid:%v esid:%v etid:%v ebid:%v type:%q",
					fs.session.id, fs.targetID, event.SessionID,
					event.TargetInfo.TargetID, event.TargetInfo.BrowserContextID,
					event.TargetInfo.Type)
				return
			default:
				k6Throw(fs.ctx, "cannot create frame session (iframe): %w", err)
			}
		}

		fs.page.attachFrameSession(cdp.FrameID(targetID), frameSession)
		return
	}

	if event.TargetInfo.Type != "worker" {
		fs.logger.Debugf("FrameSession:onAttachedToTarget returns: target is not a worker",
			"sid:%v tid:%v esid:%v etid:%v ebid:%v type:%q",
			fs.session.id, fs.targetID, event.SessionID,
			event.TargetInfo.TargetID, event.TargetInfo.BrowserContextID,
			event.TargetInfo.Type)

		// Just unblock (debugger continue) these targets and detach from them.
		session.ExecuteWithoutExpectationOnReply(fs.ctx, runtime.CommandRunIfWaitingForDebugger, nil, nil)
		session.ExecuteWithoutExpectationOnReply(fs.ctx, target.CommandDetachFromTarget, &target.DetachFromTargetParams{SessionID: session.id}, nil)
		return
	}

	// Handle new worker
	w, err := NewWorker(fs.ctx, session, targetID, event.TargetInfo.URL)
	if err != nil {
		if !fs.page.browserCtx.browser.connected && strings.Contains(err.Error(), "websocket: close 1006 (abnormal closure)") {
			// If we're no longer connected to browser, then ignore WebSocket errors
			return
		}
		select {
		case <-fs.ctx.Done():
			fs.logger.Debugf("FrameSession:onAttachedToTarget:NewWorker:<-ctx.Done",
				"sid:%v tid:%v esid:%v etid:%v ebid:%v type:%q",
				fs.session.id, fs.targetID, event.SessionID,
				event.TargetInfo.TargetID, event.TargetInfo.BrowserContextID,
				event.TargetInfo.Type)
			return
		default:
			k6Throw(fs.ctx, "cannot create frame session (worker): %w", err)
		}
	}
	fs.page.workers[session.id] = w
}

func (fs *FrameSession) onDetachedFromTarget(event *target.EventDetachedFromTarget) {
	fs.logger.Debugf("NewFrameSession:onDetachedFromTarget", "sid:%v tid:%v esid:%v", fs.session.id, fs.targetID, event.SessionID)

	fs.page.closeWorker(event.SessionID)
}

func (fs *FrameSession) onTargetCrashed(event *inspector.EventTargetCrashed) {
	fs.logger.Debugf("NewFrameSession:onTargetCrashed", "sid:%v tid:%v", fs.session.id, fs.targetID)
	fs.session.markAsCrashed()
	fs.page.didCrash()
}

func (fs *FrameSession) updateEmulateMedia(initial bool) error {
	fs.logger.Debugf("NewFrameSession:updateEmulateMedia", "sid:%v tid:%v", fs.session.id, fs.targetID)
	features := make([]*emulation.MediaFeature, 0)

	switch fs.page.colorScheme {
	case ColorSchemeLight:
		features = append(features, &emulation.MediaFeature{Name: "prefers-color-scheme", Value: "light"})
	case ColorSchemeDark:
		features = append(features, &emulation.MediaFeature{Name: "prefers-color-scheme", Value: "dark"})
	default:
		features = append(features, &emulation.MediaFeature{Name: "prefers-color-scheme", Value: ""})
	}

	switch fs.page.reducedMotion {
	case ReducedMotionReduce:
		features = append(features, &emulation.MediaFeature{Name: "prefers-reduced-motion", Value: "reduce"})
	default:
		features = append(features, &emulation.MediaFeature{Name: "prefers-reduced-motion", Value: ""})
	}

	action := emulation.SetEmulatedMedia().
		WithMedia(string(fs.page.mediaType)).
		WithFeatures(features)
	if err := action.Do(cdp.WithExecutor(fs.ctx, fs.session)); err != nil {
		return fmt.Errorf("unable to execute %T: %w", action, err)
	}
	return nil
}

func (fs *FrameSession) updateExtraHTTPHeaders(initial bool) {
	fs.logger.Debugf("NewFrameSession:updateExtraHTTPHeaders", "sid:%v tid:%v", fs.session.id, fs.targetID)

	// Merge extra headers from browser context and page, where page specific headers ake precedence.
	mergedHeaders := make(network.Headers)
	for k, v := range fs.page.browserCtx.opts.ExtraHTTPHeaders {
		mergedHeaders[k] = v
	}
	for k, v := range fs.page.extraHTTPHeaders {
		mergedHeaders[k] = v
	}
	if !initial || len(mergedHeaders) > 0 {
		fs.networkManager.SetExtraHTTPHeaders(mergedHeaders)
	}
}

func (fs *FrameSession) updateGeolocation(initial bool) error {
	fs.logger.Debugf("NewFrameSession:updateGeolocation", "sid:%v tid:%v", fs.session.id, fs.targetID)

	geolocation := fs.page.browserCtx.opts.Geolocation
	if !initial || geolocation != nil {
		action := emulation.SetGeolocationOverride().
			WithLatitude(geolocation.Latitude).
			WithLongitude(geolocation.Longitude).
			WithAccuracy(geolocation.Accurracy)
		if err := action.Do(cdp.WithExecutor(fs.ctx, fs.session)); err != nil {
			return fmt.Errorf("unable to set geolocation override: %w", err)
		}
	}
	return nil
}

func (fs *FrameSession) updateHttpCredentials(initial bool) {
	fs.logger.Debugf("NewFrameSession:updateHttpCredentials", "sid:%v tid:%v", fs.session.id, fs.targetID)

	credentials := fs.page.browserCtx.opts.HttpCredentials
	if !initial || credentials != nil {
		fs.networkManager.Authenticate(credentials)
	}
}

func (fs *FrameSession) updateOffline(initial bool) {
	fs.logger.Debugf("NewFrameSession:updateOffline", "sid:%v tid:%v", fs.session.id, fs.targetID)

	offline := fs.page.browserCtx.opts.Offline
	if !initial || offline {
		fs.networkManager.SetOfflineMode(offline)
	}
}

func (fs *FrameSession) updateRequestInterception(initial bool) error {
	fs.logger.Debugf("NewFrameSession:updateRequestInterception", "sid:%v tid:%v", fs.session.id, fs.targetID)

	return fs.networkManager.setRequestInterception(fs.page.hasRoutes())
}

func (fs *FrameSession) updateViewport() error {
	fs.logger.Debugf("NewFrameSession:updateViewport", "sid:%v tid:%v", fs.session.id, fs.targetID)

	opts := fs.page.browserCtx.opts
	emulatedSize := fs.page.emulatedSize
	if emulatedSize == nil {
		return nil
	}
	viewport := emulatedSize.Viewport
	screen := emulatedSize.Screen
	orientation := emulation.ScreenOrientation{
		Angle: 0.0,
		Type:  emulation.OrientationTypePortraitPrimary,
	}
	if viewport.Width > viewport.Height {
		orientation.Angle = 90.0
		orientation.Type = emulation.OrientationTypeLandscapePrimary
	}
	action := emulation.SetDeviceMetricsOverride(viewport.Width, viewport.Height, opts.DeviceScaleFactor, opts.IsMobile).
		WithScreenOrientation(&orientation).
		WithScreenWidth(screen.Width).
		WithScreenHeight(screen.Height)
	if err := action.Do(cdp.WithExecutor(fs.ctx, fs.session)); err != nil {
		return fmt.Errorf("unable to emulate viewport: %w", err)
	}
	action2 := browser.SetWindowBounds(fs.windowID, &browser.Bounds{
		Width:  viewport.Width,
		Height: viewport.Height,
	})
	if err := action2.Do(cdp.WithExecutor(fs.ctx, fs.session)); err != nil {
		return fmt.Errorf("unable to set window bounds: %w", err)
	}
	return nil
}
