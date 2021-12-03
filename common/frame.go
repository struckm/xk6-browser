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
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/dop251/goja"
	"github.com/grafana/xk6-browser/api"
	k6common "go.k6.io/k6/js/common"
)

// Ensure frame implements the Frame interface
var _ api.Frame = &Frame{}

type DocumentInfo struct {
	documentID string
	request    *Request
}

// Frame represents a frame in an HTML document
type Frame struct {
	BaseEventEmitter

	ctx         context.Context
	page        *Page
	manager     *FrameManager
	parentFrame *Frame

	childFramesMu sync.RWMutex
	childFrames   map[api.Frame]bool
	id            cdp.FrameID
	loaderID      string
	name          string
	url           string
	detached      bool

	// A life cycle event is only considered triggered for a frame if the entire
	// frame subtree has also had the life cycle event triggered.
	lifecycleEventsMu      sync.RWMutex
	lifecycleEvents        map[LifecycleEvent]bool
	subtreeLifecycleEvents map[LifecycleEvent]bool

	documentHandle *ElementHandle

	executionContextMu      sync.RWMutex
	mainExecutionContext    *ExecutionContext
	utilityExecutionContext *ExecutionContext

	loadingStartedTime time.Time

	networkIdleCh chan struct{}

	inflightRequestsMu sync.RWMutex
	inflightRequests   map[network.RequestID]bool

	currentDocument *DocumentInfo
	pendingDocument *DocumentInfo

	log *Logger
}

// NewFrame creates a new HTML document frame
func NewFrame(ctx context.Context, m *FrameManager, parentFrame *Frame, frameID cdp.FrameID, log *Logger) *Frame {
	if log.DebugMode() {
		pfid := ""
		if parentFrame != nil {
			pfid = parentFrame.ID()
		}
		sid := ""
		if m != nil && m.session != nil {
			sid = string(m.session.id)
		}
		log.Debugf("NewFrame", "sid:%s tid:%s ptid:%s", sid, frameID, pfid)
	}

	return &Frame{
		BaseEventEmitter:       NewBaseEventEmitter(ctx),
		ctx:                    ctx,
		page:                   m.page,
		manager:                m,
		parentFrame:            parentFrame,
		childFrames:            make(map[api.Frame]bool),
		id:                     frameID,
		lifecycleEvents:        make(map[LifecycleEvent]bool),
		subtreeLifecycleEvents: make(map[LifecycleEvent]bool),
		inflightRequests:       make(map[network.RequestID]bool),
		currentDocument:        &DocumentInfo{},
		networkIdleCh:          make(chan struct{}),
		log:                    log,
	}
}

func (f *Frame) addChildFrame(child *Frame) {
	f.log.Debugf("Frame:addChildFrame", "tid:%s ctid:%s furl:%q cfurl:%q", f.id, child.id, f.url, child.url)

	f.childFramesMu.Lock()
	defer f.childFramesMu.Unlock()

	f.childFrames[child] = true
}

func (f *Frame) addRequest(id network.RequestID) {
	f.log.Debugf("Frame:addRequest", "tid:%s furl:%q rid:%s", f.id, f.url, id)

	f.inflightRequestsMu.Lock()
	defer f.inflightRequestsMu.Unlock()

	f.inflightRequests[id] = true
}

func (f *Frame) deleteRequest(id network.RequestID) {
	f.log.Debugf("Frame:deleteRequest", "tid:%s furl:%q rid:%s", f.id, f.url, id)

	f.inflightRequestsMu.Lock()
	defer f.inflightRequestsMu.Unlock()

	delete(f.inflightRequests, id)
}

func (f *Frame) inflightRequestsLen() int {
	f.inflightRequestsMu.RLock()
	defer f.inflightRequestsMu.RUnlock()

	return len(f.inflightRequests)
}

func (f *Frame) clearLifecycle() {
	f.log.Debugf("Frame:clearLifecycle", "tid:%s furl:%q", f.id, f.url)

	// clear lifecycle events
	f.lifecycleEventsMu.Lock()
	{
		for e := range f.lifecycleEvents {
			f.lifecycleEvents[e] = false
		}
	}
	f.lifecycleEventsMu.Unlock()

	f.page.frameManager.mainFrame.recalculateLifecycle()

	// keep the request related to the document if present
	// in f.inflightRequests
	f.inflightRequestsMu.Lock()
	{
		// currentDocument may not always have a request
		// associated with it. see: frame_manager.go
		cdr := f.currentDocument.request

		inflightRequests := make(map[network.RequestID]bool)
		for req := range f.inflightRequests {
			if cdr != nil && req != cdr.requestID {
				continue
			}
			inflightRequests[req] = true
		}
		f.inflightRequests = inflightRequests
	}
	f.inflightRequestsMu.Unlock()

	f.stopNetworkIdleTimer()
	if f.inflightRequestsLen() == 0 {
		f.startNetworkIdleTimer()
	}
}

func (f *Frame) recalculateLifecycle() {
	f.log.Debugf("Frame:recalculateLifecycle", "tid:%s furl:%q", f.id, f.url)

	// Start with triggered events.
	var events map[LifecycleEvent]bool = make(map[LifecycleEvent]bool)
	f.lifecycleEventsMu.RLock()
	{
		for k, v := range f.lifecycleEvents {
			events[k] = v
		}
	}
	f.lifecycleEventsMu.RUnlock()

	// Only consider a life cycle event as fired if it has triggered for all of subtree.
	f.childFramesMu.RLock()
	{
		for child := range f.childFrames {
			cf := child.(*Frame)
			// a precaution for preventing a deadlock in *Frame.childFramesMu
			if cf == f {
				continue
			}
			cf.recalculateLifecycle()
			for k := range events {
				if !cf.hasSubtreeLifecycleEventFired(k) {
					delete(events, k)
				}
			}
		}
	}
	f.childFramesMu.RUnlock()

	// Check if any of the fired events should be considered fired when looking at the entire subtree.
	mainFrame := f.manager.MainFrame()
	for k := range events {
		if f.hasSubtreeLifecycleEventFired(k) {
			continue
		}
		f.emit(EventFrameAddLifecycle, k)

		if f != mainFrame {
			continue
		}
		switch k {
		case LifecycleEventLoad:
			f.page.emit(EventPageLoad, nil)
		case LifecycleEventDOMContentLoad:
			f.page.emit(EventPageDOMContentLoaded, nil)
		}
	}

	// Emit removal events
	f.lifecycleEventsMu.RLock()
	{
		for k := range f.subtreeLifecycleEvents {
			if ok := events[k]; !ok {
				f.emit(EventFrameRemoveLifecycle, k)
			}
		}
	}
	f.lifecycleEventsMu.RUnlock()

	f.lifecycleEventsMu.Lock()
	{
		f.subtreeLifecycleEvents = make(map[LifecycleEvent]bool)
		for k, v := range events {
			f.subtreeLifecycleEvents[k] = v
		}
	}
	f.lifecycleEventsMu.Unlock()
}

func (f *Frame) stopNetworkIdleTimer() {
	f.log.Debugf("Frame:stopNetworkIdleTimer", "tid:%s furl:%q", f.id, f.url)

	select {
	case f.networkIdleCh <- struct{}{}:
	default:
	}
}

func (f *Frame) startNetworkIdleTimer() {
	f.log.Debugf("Frame:startNetworkIdleTimer", "tid:%s furl:%q", f.id, f.url)

	if f.hasLifecycleEventFired(LifecycleEventNetworkIdle) || f.detached {
		return
	}

	f.stopNetworkIdleTimer()

	go func() {
		select {
		case <-f.ctx.Done():
		case <-f.networkIdleCh:
		case <-time.After(LifeCycleNetworkIdleTimeout):
			f.manager.frameLifecycleEvent(f.id, LifecycleEventNetworkIdle)
		}
	}()
}

func (f *Frame) detach() {
	f.log.Debugf("Frame:detach", "tid:%s furl:%q", f.id, f.url)

	f.stopNetworkIdleTimer()
	f.detached = true
	if f.parentFrame != nil {
		f.parentFrame.removeChildFrame(f)
	}
	f.parentFrame = nil
	if f.documentHandle != nil {
		f.documentHandle.Dispose()
	}
}

func (f *Frame) defaultTimeout() time.Duration {
	return time.Duration(f.manager.timeoutSettings.timeout()) * time.Second
}

func (f *Frame) document() (*ElementHandle, error) {
	f.log.Debugf("Frame:document", "tid:%s furl:%q", f.id, f.url)

	if f.documentHandle != nil {
		return f.documentHandle, nil
	}

	f.waitForExecutionContext(mainExecutionContext)

	var (
		result interface{}
		err    error
	)
	rt := k6common.GetRuntime(f.ctx)
	f.executionContextMu.RLock()
	{
		result, err = f.mainExecutionContext.evaluate(f.ctx, false, false, rt.ToValue("document"), nil)
	}
	f.executionContextMu.RUnlock()
	if err != nil {
		return nil, err
	}
	f.documentHandle = result.(*ElementHandle)
	return f.documentHandle, err
}

func (f *Frame) hasContext(world string) bool {
	f.executionContextMu.RLock()
	defer f.executionContextMu.RUnlock()

	switch world {
	case mainExecutionContext:
		return f.mainExecutionContext != nil
	case utilityExecutionContext:
		return f.utilityExecutionContext != nil
	}
	return false // Should never reach here!
}

func (f *Frame) hasLifecycleEventFired(event LifecycleEvent) bool {
	f.lifecycleEventsMu.RLock()
	defer f.lifecycleEventsMu.RUnlock()

	return f.lifecycleEvents[event]
}

func (f *Frame) hasSubtreeLifecycleEventFired(event LifecycleEvent) bool {
	f.lifecycleEventsMu.RLock()
	defer f.lifecycleEventsMu.RUnlock()

	return f.subtreeLifecycleEvents[event]
}

func (f *Frame) navigated(name string, url string, loaderID string) {
	f.log.Debugf("Frame:navigated", "tid:%s lid:%s furl:%q name:%q url:%q", f.id, loaderID, f.url, name, url)

	f.name = name
	f.url = url
	f.loaderID = loaderID
	f.page.emit(EventPageFrameNavigated, f)
}

func (f *Frame) nullContext(id runtime.ExecutionContextID) {
	f.log.Debugf("Frame:nullContext", "tid:%s ecid:%d furl:%q", f.id, id, f.url)

	f.executionContextMu.Lock()
	defer f.executionContextMu.Unlock()

	if f.mainExecutionContext != nil && f.mainExecutionContext.id == id {
		f.mainExecutionContext = nil
		f.documentHandle = nil
	} else if f.utilityExecutionContext != nil && f.utilityExecutionContext.id == id {
		f.utilityExecutionContext = nil
	}
}

func (f *Frame) onLifecycleEvent(event LifecycleEvent) {
	f.log.Debugf("Frame:onLifecycleEvent", "tid:%s furl:%q event:%s", f.id, f.url, event)

	f.lifecycleEventsMu.Lock()
	defer f.lifecycleEventsMu.Unlock()

	if ok := f.lifecycleEvents[event]; ok {
		return
	}
	f.lifecycleEvents[event] = true
}

func (f *Frame) onLoadingStarted() {
	f.log.Debugf("Frame:onLoadingStarted", "tid:%s furl:%q", f.id, f.url)

	f.loadingStartedTime = time.Now()
}

func (f *Frame) onLoadingStopped() {
	f.log.Debugf("Frame:onLoadingStopped", "tid:%s furl:%q", f.id, f.url)

	f.lifecycleEventsMu.Lock()
	defer f.lifecycleEventsMu.Unlock()

	f.lifecycleEvents[LifecycleEventDOMContentLoad] = true
	f.lifecycleEvents[LifecycleEventLoad] = true
	f.lifecycleEvents[LifecycleEventNetworkIdle] = true
}

func (f *Frame) position() *Position {
	frame := f.manager.getFrameByID(cdp.FrameID(f.page.targetID))
	if frame == nil {
		return nil
	}
	if frame == f.page.frameManager.mainFrame {
		return &Position{X: 0, Y: 0}
	}
	element := frame.FrameElement()
	box := element.BoundingBox()
	return &Position{X: box.X, Y: box.Y}
}

func (f *Frame) removeChildFrame(child *Frame) {
	f.log.Debugf("Frame:removeChildFrame", "tid:%s ctid:%s furl:%q curl:%q", f.id, child.id, f.url, child.url)

	f.childFramesMu.Lock()
	defer f.childFramesMu.Unlock()

	delete(f.childFrames, child)
}

func (f *Frame) requestByID(reqID network.RequestID) *Request {
	frameSession := f.page.getFrameSession(f.id)
	if frameSession == nil {
		frameSession = f.page.mainFrameSession
	}
	return frameSession.networkManager.requestFromID(reqID)
}

func (f *Frame) setContext(world string, execCtx *ExecutionContext) {
	f.log.Debugf("Frame:setContext", "tid:%s ecid:%d world:%s furl:%q", f.id, execCtx.id, world, f.url)

	f.executionContextMu.Lock()
	defer f.executionContextMu.Unlock()

	switch world {
	case mainExecutionContext:
		if f.mainExecutionContext == nil {
			f.mainExecutionContext = execCtx
		}
	case utilityExecutionContext:
		if f.utilityExecutionContext == nil {
			f.utilityExecutionContext = execCtx
		}
	default:
		err := fmt.Errorf("unknown world: %q, it should be either main or utility", world)
		panic(err)
	}
}

func (f *Frame) setID(id cdp.FrameID) {
	f.id = id
}

func (f *Frame) waitForExecutionContext(world string) {
	f.log.Debugf("Frame:waitForExecutionContext", "tid:%s furl:%q world:%s", f.id, f.url, world)
	defer f.log.Debugf("Frame:waitForExecutionContext:return", "tid:%s furl:%q world:%s", f.id, f.url, world)

	wait := func(done chan struct{}) {
		var ok bool
		select {
		case <-f.ctx.Done():
			ok = true
		default:
			ok = f.hasContext(world)
		}
		if !ok {
			// TODO: change sleeping with something else
			time.Sleep(time.Millisecond * 50)
			return
		}
		done <- struct{}{}
	}

	done := make(chan struct{})
	go func() {
		for {
			wait(done)
		}
	}()
	<-done
}

func (f *Frame) waitForFunction(apiCtx context.Context, world string, predicateFn goja.Value, polling PollingType, interval int64, timeout time.Duration, args ...goja.Value) (interface{}, error) {
	f.log.Debugf("Frame:waitForFunction", "tid:%s furl:%q world:%s pt:%s to:%s", f.id, f.url, world, polling, timeout)

	rt := k6common.GetRuntime(f.ctx)
	f.waitForExecutionContext(world)

	f.executionContextMu.RLock()
	defer f.executionContextMu.RUnlock()

	execCtx := f.mainExecutionContext
	if world == utilityExecutionContext {
		execCtx = f.utilityExecutionContext
	}
	injected, err := execCtx.getInjectedScript(apiCtx)
	if err != nil {
		return nil, err
	}
	pageFn := rt.ToValue(`
		(injected, predicate, polling, timeout, ...args) => {
			return injected.waitForPredicateFunction(predicate, polling, timeout, ...args);
		}
	`)
	predicate := ""
	_, isCallable := goja.AssertFunction(predicateFn)
	if !isCallable {
		predicate = fmt.Sprintf("return (%s);", predicateFn.ToString().String())
	} else {
		predicate = fmt.Sprintf("return (%s)(...args);", predicateFn.ToString().String())
	}
	result, err := execCtx.evaluate(
		apiCtx, true, false, pageFn, append([]goja.Value{
			rt.ToValue(injected),
			rt.ToValue(predicate),
			rt.ToValue(polling),
		}, args...)...)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (f *Frame) waitForSelector(selector string, opts *FrameWaitForSelectorOptions) (*ElementHandle, error) {
	f.log.Debugf("Frame:waitForSelector", "tid:%s furl:%q sel:%q", f.id, f.url, selector)

	document, err := f.document()
	if err != nil {
		return nil, err
	}

	handle, err := document.waitForSelector(f.ctx, selector, opts)
	if err != nil {
		return nil, err
	}
	if handle == nil {
		return nil, errors.New("wait for selector didn't resulted in any nodes")
	}

	// We always return ElementHandles in the main execution context (aka "DOM world")
	f.executionContextMu.RLock()
	defer f.executionContextMu.RUnlock()

	if handle.execCtx != f.mainExecutionContext {
		defer handle.Dispose()
		handle, err = f.mainExecutionContext.adoptElementHandle(handle)
		if err != nil {
			return nil, err
		}
	}

	return handle, nil
}

func (f *Frame) AddScriptTag(opts goja.Value) {
	rt := k6common.GetRuntime(f.ctx)
	k6common.Throw(rt, errors.New("Frame.AddScriptTag() has not been implemented yet"))
	applySlowMo(f.ctx)
}

func (f *Frame) AddStyleTag(opts goja.Value) {
	rt := k6common.GetRuntime(f.ctx)
	k6common.Throw(rt, errors.New("Frame.AddStyleTag() has not been implemented yet"))
	applySlowMo(f.ctx)
}

// Check clicks the first element found that matches selector
func (f *Frame) Check(selector string, opts goja.Value) {
	f.log.Debugf("Frame:Check", "tid:%s furl:%q sel:%q", f.id, f.url, selector)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameCheckOptions(f.defaultTimeout())
	err := parsedOpts.Parse(f.ctx, opts)
	if err != nil {
		k6common.Throw(rt, err)
	}

	fn := func(apiCtx context.Context, handle *ElementHandle, p *Position) (interface{}, error) {
		return nil, handle.setChecked(apiCtx, true, p)
	}
	actFn := framePointerActionFn(f, selector, DOMElementStateAttached, parsedOpts.Strict, fn, &parsedOpts.ElementHandleBasePointerOptions)
	_, err = callApiWithTimeout(f.ctx, actFn, parsedOpts.Timeout)
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
}

// ChildFrames returns a list of child frames
func (f *Frame) ChildFrames() []api.Frame {
	f.childFramesMu.RLock()
	defer f.childFramesMu.RUnlock()

	l := make([]api.Frame, 0, len(f.childFrames))
	for child := range f.childFrames {
		l = append(l, child)
	}
	return l
}

// Click clicks the first element found that matches selector
func (f *Frame) Click(selector string, opts goja.Value) {
	f.log.Debugf("Frame:Click", "tid:%s furl:%q sel:%q", f.id, f.url, selector)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameClickOptions(f.defaultTimeout())
	err := parsedOpts.Parse(f.ctx, opts)
	if err != nil {
		k6common.Throw(rt, err)
	}

	fn := func(apiCtx context.Context, handle *ElementHandle, p *Position) (interface{}, error) {
		return nil, handle.click(p, parsedOpts.ToMouseClickOptions())
	}
	actFn := framePointerActionFn(f, selector, DOMElementStateAttached, parsedOpts.Strict, fn, &parsedOpts.ElementHandleBasePointerOptions)
	_, err = callApiWithTimeout(f.ctx, actFn, parsedOpts.Timeout)
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
}

// Content returns the HTML content of the frame
func (f *Frame) Content() string {
	f.log.Debugf("Frame:Content", "tid:%s furl:%q", f.id, f.url)

	rt := k6common.GetRuntime(f.ctx)
	js := `let content = '';
		if (document.doctype) {
			content = new XMLSerializer().serializeToString(document.doctype);
		}
		if (document.documentElement) {
			content += document.documentElement.outerHTML;
		}
		return content;`
	return f.Evaluate(rt.ToValue(js)).(string)
}

// Dblclick double clicks an element matching provided selector
func (f *Frame) Dblclick(selector string, opts goja.Value) {
	f.log.Debugf("Frame:DblClick", "tid:%s furl:%q sel:%q", f.id, f.url, selector)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameDblClickOptions(f.defaultTimeout())
	err := parsedOpts.Parse(f.ctx, opts)
	if err != nil {
		k6common.Throw(rt, err)
	}

	fn := func(apiCtx context.Context, handle *ElementHandle, p *Position) (interface{}, error) {
		return nil, handle.dblClick(p, parsedOpts.ToMouseClickOptions())
	}
	actFn := framePointerActionFn(f, selector, DOMElementStateAttached, parsedOpts.Strict, fn, &parsedOpts.ElementHandleBasePointerOptions)
	_, err = callApiWithTimeout(f.ctx, actFn, parsedOpts.Timeout)
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
}

func (f *Frame) DispatchEvent(selector string, typ string, eventInit goja.Value, opts goja.Value) {
	f.log.Debugf("Frame:DispatchEvent", "tid:%s furl:%q sel:%q typ:%s", f.id, f.url, selector, typ)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameDblClickOptions(f.defaultTimeout())
	if err := parsedOpts.Parse(f.ctx, opts); err != nil {
		k6common.Throw(rt, err)
	}

	fn := func(apiCtx context.Context, handle *ElementHandle) (interface{}, error) {
		return handle.dispatchEvent(apiCtx, typ, eventInit)
	}
	actFn := frameActionFn(f, selector, DOMElementStateAttached, parsedOpts.Strict, fn, []string{}, parsedOpts.Force, parsedOpts.NoWaitAfter, parsedOpts.Timeout)
	_, err := callApiWithTimeout(f.ctx, actFn, parsedOpts.Timeout)
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
}

// Evaluate will evaluate provided page function within an execution context
func (f *Frame) Evaluate(pageFunc goja.Value, args ...goja.Value) (result interface{}) {
	f.log.Debugf("Frame:Evaluate", "tid:%s furl:%q", f.id, f.url)

	f.waitForExecutionContext(mainExecutionContext)

	var (
		rt  = k6common.GetRuntime(f.ctx)
		err error
	)
	f.executionContextMu.RLock()
	{
		result, err = f.mainExecutionContext.Evaluate(f.ctx, pageFunc, args...)
	}
	f.executionContextMu.RUnlock()
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
	return result
}

// EvaluateHandle will evaluate provided page function within an execution context
func (f *Frame) EvaluateHandle(pageFunc goja.Value, args ...goja.Value) (handle api.JSHandle) {
	f.log.Debugf("Frame:EvaluateHandle", "tid:%s furl:%q", f.id, f.url)

	f.waitForExecutionContext(mainExecutionContext)

	var (
		rt  = k6common.GetRuntime(f.ctx)
		err error
	)
	f.executionContextMu.RLock()
	{
		handle, err = f.mainExecutionContext.EvaluateHandle(f.ctx, pageFunc, args...)
	}
	f.executionContextMu.RUnlock()
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
	return handle
}

func (f *Frame) Fill(selector string, value string, opts goja.Value) {
	f.log.Debugf("Frame:Fill", "tid:%s furl:%q sel:%q val:%s", f.id, f.url, selector, value)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameFillOptions(f.defaultTimeout())
	if err := parsedOpts.Parse(f.ctx, opts); err != nil {
		k6common.Throw(rt, err)
	}

	fn := func(apiCtx context.Context, handle *ElementHandle) (interface{}, error) {
		return handle.fill(apiCtx, value)
	}
	actFn := frameActionFn(f, selector, DOMElementStateAttached, parsedOpts.Strict, fn, []string{"visible", "enabled", "editable"}, parsedOpts.Force, parsedOpts.NoWaitAfter, parsedOpts.Timeout)
	_, err := callApiWithTimeout(f.ctx, actFn, parsedOpts.Timeout)
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
}

// Focus fetches an element with selector and focuses it
func (f *Frame) Focus(selector string, opts goja.Value) {
	f.log.Debugf("Frame:Focus", "tid:%s furl:%q sel:%q", f.id, f.url, selector)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameBaseOptions(f.defaultTimeout())
	if err := parsedOpts.Parse(f.ctx, opts); err != nil {
		k6common.Throw(rt, err)
	}

	fn := func(apiCtx context.Context, handle *ElementHandle) (interface{}, error) {
		return nil, handle.focus(apiCtx, true)
	}
	actFn := frameActionFn(f, selector, DOMElementStateAttached, parsedOpts.Strict, fn, []string{}, false, true, parsedOpts.Timeout)
	_, err := callApiWithTimeout(f.ctx, actFn, parsedOpts.Timeout)
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
}

func (f *Frame) FrameElement() api.ElementHandle {
	f.log.Debugf("Frame:FrameElement", "tid:%s furl:%q", f.id, f.url)

	rt := k6common.GetRuntime(f.ctx)
	element, err := f.page.getFrameElement(f)
	if err != nil {
		k6common.Throw(rt, err)
	}
	return element
}

func (f *Frame) GetAttribute(selector string, name string, opts goja.Value) goja.Value {
	f.log.Debugf("Frame:GetAttribute", "tid:%s furl:%q sel:%q name:%s", f.id, f.url, selector, name)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameBaseOptions(f.defaultTimeout())
	if err := parsedOpts.Parse(f.ctx, opts); err != nil {
		k6common.Throw(rt, err)
	}

	fn := func(apiCtx context.Context, handle *ElementHandle) (interface{}, error) {
		return handle.getAttribute(apiCtx, name)
	}
	actFn := frameActionFn(f, selector, DOMElementStateAttached, parsedOpts.Strict, fn, []string{}, false, true, parsedOpts.Timeout)
	value, err := callApiWithTimeout(f.ctx, actFn, parsedOpts.Timeout)
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
	return value.(goja.Value)
}

// Goto will navigate the frame to the specified URL and return a HTTP response object
func (f *Frame) Goto(url string, opts goja.Value) api.Response {
	resp := f.manager.NavigateFrame(f, url, opts)
	applySlowMo(f.ctx)
	return resp
}

// Hover hovers an element identified by provided selector
func (f *Frame) Hover(selector string, opts goja.Value) {
	f.log.Debugf("Frame:Hover", "tid:%s furl:%q sel:%q", f.id, f.url, selector)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameHoverOptions(f.defaultTimeout())
	err := parsedOpts.Parse(f.ctx, opts)
	if err != nil {
		k6common.Throw(rt, err)
	}

	fn := func(apiCtx context.Context, handle *ElementHandle, p *Position) (interface{}, error) {
		return nil, handle.hover(apiCtx, p)
	}
	actFn := framePointerActionFn(f, selector, DOMElementStateAttached, parsedOpts.Strict, fn, &parsedOpts.ElementHandleBasePointerOptions)
	_, err = callApiWithTimeout(f.ctx, actFn, parsedOpts.Timeout)
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
}

func (f *Frame) InnerHTML(selector string, opts goja.Value) string {
	f.log.Debugf("Frame:InnerHTML", "tid:%s furl:%q sel:%q", f.id, f.url, selector)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameInnerHTMLOptions(f.defaultTimeout())
	if err := parsedOpts.Parse(f.ctx, opts); err != nil {
		k6common.Throw(rt, err)
	}

	fn := func(apiCtx context.Context, handle *ElementHandle) (interface{}, error) {
		return handle.innerHTML(apiCtx)
	}
	actFn := frameActionFn(f, selector, DOMElementStateAttached, parsedOpts.Strict, fn, []string{}, false, true, parsedOpts.Timeout)
	value, err := callApiWithTimeout(f.ctx, actFn, parsedOpts.Timeout)
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
	return value.(string)
}

func (f *Frame) InnerText(selector string, opts goja.Value) string {
	f.log.Debugf("Frame:InnerText", "tid:%s furl:%q sel:%q", f.id, f.url, selector)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameInnerHTMLOptions(f.defaultTimeout())
	if err := parsedOpts.Parse(f.ctx, opts); err != nil {
		k6common.Throw(rt, err)
	}

	fn := func(apiCtx context.Context, handle *ElementHandle) (interface{}, error) {
		return handle.innerText(apiCtx)
	}
	actFn := frameActionFn(f, selector, DOMElementStateAttached, parsedOpts.Strict, fn, []string{}, false, true, parsedOpts.Timeout)
	value, err := callApiWithTimeout(f.ctx, actFn, parsedOpts.Timeout)
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
	return value.(string)
}

func (f *Frame) InputValue(selector string, opts goja.Value) string {
	f.log.Debugf("Frame:InputValue", "tid:%s furl:%q sel:%q", f.id, f.url, selector)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameInputValueOptions(f.defaultTimeout())
	if err := parsedOpts.Parse(f.ctx, opts); err != nil {
		k6common.Throw(rt, err)
	}

	fn := func(apiCtx context.Context, handle *ElementHandle) (interface{}, error) {
		return handle.inputValue(apiCtx)
	}
	actFn := frameActionFn(f, selector, DOMElementStateAttached, parsedOpts.Strict, fn, []string{}, false, true, parsedOpts.Timeout)
	value, err := callApiWithTimeout(f.ctx, actFn, parsedOpts.Timeout)
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
	return value.(goja.Value).String()
}

func (f *Frame) IsChecked(selector string, opts goja.Value) bool {
	f.log.Debugf("Frame:IsChecked", "tid:%s furl:%q sel:%q", f.id, f.url, selector)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameIsCheckedOptions(f.defaultTimeout())
	if err := parsedOpts.Parse(f.ctx, opts); err != nil {
		k6common.Throw(rt, err)
	}

	fn := func(apiCtx context.Context, handle *ElementHandle) (interface{}, error) {
		value, err := handle.isChecked(apiCtx, 0) // Zero timeout when checking state
		if err == ErrTimedOut {                   // We don't care about timeout errors here!
			return value, nil
		}
		return value, err
	}
	actFn := frameActionFn(f, selector, DOMElementStateAttached, parsedOpts.Strict, fn, []string{}, false, true, parsedOpts.Timeout)
	value, err := callApiWithTimeout(f.ctx, actFn, parsedOpts.Timeout)
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
	return value.(bool)
}

// IsDetached returns whether the frame is detached or not
func (f *Frame) IsDetached() bool {
	return f.detached
}

func (f *Frame) IsDisabled(selector string, opts goja.Value) bool {
	f.log.Debugf("Frame:IsDisabled", "tid:%s furl:%q sel:%q", f.id, f.url, selector)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameIsDisabledOptions(f.defaultTimeout())
	if err := parsedOpts.Parse(f.ctx, opts); err != nil {
		k6common.Throw(rt, err)
	}

	fn := func(apiCtx context.Context, handle *ElementHandle) (interface{}, error) {
		value, err := handle.isDisabled(apiCtx, 0) // Zero timeout when checking state
		if err == ErrTimedOut {                    // We don't care about timeout errors here!
			return value, nil
		}
		return value, err
	}
	actFn := frameActionFn(f, selector, DOMElementStateAttached, parsedOpts.Strict, fn, []string{}, false, true, parsedOpts.Timeout)
	value, err := callApiWithTimeout(f.ctx, actFn, parsedOpts.Timeout)
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
	return value.(bool)
}

func (f *Frame) IsEditable(selector string, opts goja.Value) bool {
	f.log.Debugf("Frame:IsEditable", "tid:%s furl:%q sel:%q", f.id, f.url, selector)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameIsEditableOptions(f.defaultTimeout())
	if err := parsedOpts.Parse(f.ctx, opts); err != nil {
		k6common.Throw(rt, err)
	}

	fn := func(apiCtx context.Context, handle *ElementHandle) (interface{}, error) {
		value, err := handle.isEditable(apiCtx, 0) // Zero timeout when checking state
		if err == ErrTimedOut {                    // We don't care about timeout errors here!
			return value, nil
		}
		return value, err
	}
	actFn := frameActionFn(f, selector, DOMElementStateAttached, parsedOpts.Strict, fn, []string{}, false, true, parsedOpts.Timeout)
	value, err := callApiWithTimeout(f.ctx, actFn, parsedOpts.Timeout)
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
	return value.(bool)
}

func (f *Frame) IsEnabled(selector string, opts goja.Value) bool {
	f.log.Debugf("Frame:IsEnabled", "tid:%s furl:%q sel:%q", f.id, f.url, selector)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameIsEnabledOptions(f.defaultTimeout())
	if err := parsedOpts.Parse(f.ctx, opts); err != nil {
		k6common.Throw(rt, err)
	}

	fn := func(apiCtx context.Context, handle *ElementHandle) (interface{}, error) {
		value, err := handle.isEnabled(apiCtx, 0) // Zero timeout when checking state
		if err == ErrTimedOut {                   // We don't care about timeout errors here!
			return value, nil
		}
		return value, err
	}
	actFn := frameActionFn(f, selector, DOMElementStateAttached, parsedOpts.Strict, fn, []string{}, false, true, parsedOpts.Timeout)
	value, err := callApiWithTimeout(f.ctx, actFn, parsedOpts.Timeout)
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
	return value.(bool)
}

func (f *Frame) IsHidden(selector string, opts goja.Value) bool {
	f.log.Debugf("Frame:IsHidden", "tid:%s furl:%q sel:%q", f.id, f.url, selector)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameIsHiddenOptions(f.defaultTimeout())
	if err := parsedOpts.Parse(f.ctx, opts); err != nil {
		k6common.Throw(rt, err)
	}

	fn := func(apiCtx context.Context, handle *ElementHandle) (interface{}, error) {
		value, err := handle.isHidden(apiCtx, 0) // Zero timeout when checking state
		if err == ErrTimedOut {                  // We don't care about timeout errors here!
			return value, nil
		}
		return value, err
	}
	actFn := frameActionFn(f, selector, DOMElementStateAttached, parsedOpts.Strict, fn, []string{}, false, true, parsedOpts.Timeout)
	value, err := callApiWithTimeout(f.ctx, actFn, parsedOpts.Timeout)
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
	return value.(bool)
}

func (f *Frame) IsVisible(selector string, opts goja.Value) bool {
	f.log.Debugf("Frame:IsVisible", "tid:%s furl:%q sel:%q", f.id, f.url, selector)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameIsVisibleOptions(f.defaultTimeout())
	if err := parsedOpts.Parse(f.ctx, opts); err != nil {
		k6common.Throw(rt, err)
	}

	fn := func(apiCtx context.Context, handle *ElementHandle) (interface{}, error) {
		value, err := handle.isVisible(apiCtx, 0) // Zero timeout when checking state
		if err == ErrTimedOut {                   // We don't care about timeout errors here!
			return value, nil
		}
		return value, err
	}
	actFn := frameActionFn(f, selector, DOMElementStateAttached, parsedOpts.Strict, fn, []string{}, false, true, parsedOpts.Timeout)
	value, err := callApiWithTimeout(f.ctx, actFn, parsedOpts.Timeout)
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
	return value.(bool)
}

// ID returns the frame id
func (f *Frame) ID() string {
	return f.id.String()
}

// LoaderID returns the ID of the frame that loaded this frame
func (f *Frame) LoaderID() string {
	return f.loaderID
}

// Name returns the frame name
func (f *Frame) Name() string {
	return f.name
}

// Query runs a selector query against the document tree, returning the first matching element or
// "null" if no match is found
func (f *Frame) Query(selector string) api.ElementHandle {
	f.log.Debugf("Frame:Query", "tid:%s furl:%q sel:%q", f.id, f.url, selector)

	rt := k6common.GetRuntime(f.ctx)
	document, err := f.document()
	if err != nil {
		k6common.Throw(rt, err)
	}
	value := document.Query(selector)
	if value != nil {
		return value
	}
	return nil
}

func (f *Frame) QueryAll(selector string) []api.ElementHandle {
	f.log.Debugf("Frame:QueryAll", "tid:%s furl:%q sel:%q", f.id, f.url, selector)

	rt := k6common.GetRuntime(f.ctx)
	document, err := f.document()
	if err != nil {
		k6common.Throw(rt, err)
	}
	value := document.QueryAll(selector)
	if value != nil {
		return value
	}
	return nil
}

// Page returns page that owns frame
func (f *Frame) Page() api.Page {
	return f.manager.page
}

// ParentFrame returns the parent frame, if one exists
func (f *Frame) ParentFrame() api.Frame {
	return f.parentFrame
}

func (f *Frame) Press(selector string, key string, opts goja.Value) {
	f.log.Debugf("Frame:Press", "tid:%s furl:%q sel:%q", f.id, f.url, selector)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFramePressOptions(f.defaultTimeout())
	if err := parsedOpts.Parse(f.ctx, opts); err != nil {
		k6common.Throw(rt, err)
	}

	fn := func(apiCtx context.Context, handle *ElementHandle) (interface{}, error) {
		return nil, handle.press(apiCtx, key, parsedOpts.ToKeyboardOptions())
	}
	actFn := frameActionFn(f, selector, DOMElementStateAttached, parsedOpts.Strict, fn, []string{}, false, parsedOpts.NoWaitAfter, parsedOpts.Timeout)
	_, err := callApiWithTimeout(f.ctx, actFn, parsedOpts.Timeout)
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
}

func (f *Frame) SelectOption(selector string, values goja.Value, opts goja.Value) []string {
	f.log.Debugf("Frame:SelectOption", "tid:%s furl:%q sel:%q", f.id, f.url, selector)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameSelectOptionOptions(f.defaultTimeout())
	if err := parsedOpts.Parse(f.ctx, opts); err != nil {
		k6common.Throw(rt, err)
	}

	fn := func(apiCtx context.Context, handle *ElementHandle) (interface{}, error) {
		return handle.selectOption(apiCtx, values)
	}
	actFn := frameActionFn(f, selector, DOMElementStateAttached, parsedOpts.Strict, fn, []string{}, parsedOpts.Force, parsedOpts.NoWaitAfter, parsedOpts.Timeout)
	value, err := callApiWithTimeout(f.ctx, actFn, parsedOpts.Timeout)
	if err != nil {
		k6common.Throw(rt, err)
	}

	arrayHandle, ok := value.(api.JSHandle)
	if !ok {
		k6common.Throw(rt, err)
	}
	properties := arrayHandle.GetProperties()
	strArr := make([]string, 0, len(properties))
	for _, property := range properties {
		strArr = append(strArr, property.JSONValue().String())
		property.Dispose()
	}
	arrayHandle.Dispose()

	applySlowMo(f.ctx)
	return strArr
}

// SetContent replaces the entire HTML document content
func (f *Frame) SetContent(html string, opts goja.Value) {
	f.log.Debugf("Frame:SetContent", "tid:%s furl:%q", f.id, f.url)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameSetContentOptions(f.defaultTimeout())
	if err := parsedOpts.Parse(f.ctx, opts); err != nil {
		k6common.Throw(rt, fmt.Errorf("failed parsing options: %w", err))
	}

	js := `(html) => {
		window.stop();
		document.open();
		document.write(html);
		document.close();
	}`
	f.waitForExecutionContext("utility")
	_, err := f.utilityExecutionContext.evaluate(f.ctx, true, true, rt.ToValue(js), rt.ToValue(html))
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
}

func (f *Frame) SetInputFiles(selector string, files goja.Value, opts goja.Value) {
	rt := k6common.GetRuntime(f.ctx)
	k6common.Throw(rt, errors.New("Frame.setInputFiles(selector, files, opts) has not been implemented yet"))
	// TODO: needs slowMo
}

func (f *Frame) Tap(selector string, opts goja.Value) {
	f.log.Debugf("Frame:Tap", "tid:%s furl:%q sel:%q", f.id, f.url, selector)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameTapOptions(f.defaultTimeout())
	err := parsedOpts.Parse(f.ctx, opts)
	if err != nil {
		k6common.Throw(rt, err)
	}

	fn := func(apiCtx context.Context, handle *ElementHandle, p *Position) (interface{}, error) {
		return nil, handle.tap(apiCtx, p)
	}
	actFn := framePointerActionFn(f, selector, DOMElementStateAttached, parsedOpts.Strict, fn, &parsedOpts.ElementHandleBasePointerOptions)
	_, err = callApiWithTimeout(f.ctx, actFn, parsedOpts.Timeout)
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
}

func (f *Frame) TextContent(selector string, opts goja.Value) string {
	f.log.Debugf("Frame:TextContent", "tid:%s furl:%q sel:%q", f.id, f.url, selector)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameTextContentOptions(f.defaultTimeout())
	if err := parsedOpts.Parse(f.ctx, opts); err != nil {
		k6common.Throw(rt, err)
	}

	fn := func(apiCtx context.Context, handle *ElementHandle) (interface{}, error) {
		return handle.textContent(apiCtx)
	}
	actFn := frameActionFn(f, selector, DOMElementStateAttached, parsedOpts.Strict, fn, []string{}, false, true, parsedOpts.Timeout)
	value, err := callApiWithTimeout(f.ctx, actFn, parsedOpts.Timeout)
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
	return value.(string)
}

func (f *Frame) Title() string {
	f.log.Debugf("Frame:Title", "tid:%s furl:%q", f.id, f.url)

	rt := k6common.GetRuntime(f.ctx)
	return f.Evaluate(rt.ToValue("document.title")).(string)
}

func (f *Frame) Type(selector string, text string, opts goja.Value) {
	f.log.Debugf("Frame:Type", "tid:%s furl:%q sel:%q text:%s", f.id, f.url, selector, text)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameTypeOptions(f.defaultTimeout())
	if err := parsedOpts.Parse(f.ctx, opts); err != nil {
		k6common.Throw(rt, err)
	}

	fn := func(apiCtx context.Context, handle *ElementHandle) (interface{}, error) {
		return nil, handle.typ(apiCtx, text, parsedOpts.ToKeyboardOptions())
	}
	actFn := frameActionFn(f, selector, DOMElementStateAttached, parsedOpts.Strict, fn, []string{}, false, parsedOpts.NoWaitAfter, parsedOpts.Timeout)
	_, err := callApiWithTimeout(f.ctx, actFn, parsedOpts.Timeout)
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
}

func (f *Frame) Uncheck(selector string, opts goja.Value) {
	f.log.Debugf("Frame:Uncheck", "tid:%s furl:%q sel:%q", f.id, f.url, selector)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameUncheckOptions(f.defaultTimeout())
	err := parsedOpts.Parse(f.ctx, opts)
	if err != nil {
		k6common.Throw(rt, err)
	}

	fn := func(apiCtx context.Context, handle *ElementHandle, p *Position) (interface{}, error) {
		return nil, handle.setChecked(apiCtx, false, p)
	}
	actFn := framePointerActionFn(f, selector, DOMElementStateAttached, parsedOpts.Strict, fn, &parsedOpts.ElementHandleBasePointerOptions)
	_, err = callApiWithTimeout(f.ctx, actFn, parsedOpts.Timeout)
	if err != nil {
		k6common.Throw(rt, err)
	}

	applySlowMo(f.ctx)
}

// URL returns the frame URL
func (f *Frame) URL() string {
	return f.url
}

// WaitForFunction waits for the given predicate to return a truthy value
func (f *Frame) WaitForFunction(pageFunc goja.Value, opts goja.Value, args ...goja.Value) api.JSHandle {
	f.log.Debugf("Frame:WaitForFunction", "tid:%s furl:%q", f.id, f.url)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameWaitForFunctionOptions(f.defaultTimeout())
	err := parsedOpts.Parse(f.ctx, opts)
	if err != nil {
		k6common.Throw(rt, fmt.Errorf("failed parsing options: %w", err))
	}

	handle, err := f.waitForFunction(f.ctx, utilityExecutionContext, pageFunc, parsedOpts.Polling, parsedOpts.Interval, parsedOpts.Timeout, args...)
	if err != nil {
		k6common.Throw(rt, err)
	}
	return handle.(api.JSHandle)
}

// WaitForLoadState waits for the given load state to be reached
func (f *Frame) WaitForLoadState(state string, opts goja.Value) {
	f.log.Debugf("Frame:WaitForLoadState", "tid:%s furl:%q state:%s", f.id, f.url, state)
	defer f.log.Debugf("Frame:WaitForLoadState:return", "tid:%s furl:%q state:%s", f.id, f.url, state)

	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameWaitForLoadStateOptions(f.defaultTimeout())
	err := parsedOpts.Parse(f.ctx, opts)
	if err != nil {
		k6common.Throw(rt, fmt.Errorf("failed parsing options: %w", err))
	}

	waitUntil := LifecycleEventLoad
	switch state {
	case "domcontentloaded":
		waitUntil = LifecycleEventDOMContentLoad
	case "networkidle":
		waitUntil = LifecycleEventNetworkIdle
	}

	if f.hasLifecycleEventFired(waitUntil) {
		return
	}

	waitForEvent(f.ctx, f, []string{EventFrameAddLifecycle}, func(data interface{}) bool {
		return data.(LifecycleEvent) == waitUntil
	}, parsedOpts.Timeout)
}

// WaitForNavigation waits for the given navigation lifecycle event to happen
func (f *Frame) WaitForNavigation(opts goja.Value) api.Response {
	return f.manager.WaitForFrameNavigation(f, opts)
}

// WaitForSelector waits for the given selector to match the waiting criteria
func (f *Frame) WaitForSelector(selector string, opts goja.Value) api.ElementHandle {
	rt := k6common.GetRuntime(f.ctx)
	parsedOpts := NewFrameWaitForSelectorOptions(f.defaultTimeout())
	if err := parsedOpts.Parse(f.ctx, opts); err != nil {
		k6common.Throw(rt, fmt.Errorf("failed parsing options: %w", err))
	}
	handle, err := f.waitForSelector(selector, parsedOpts)
	if err != nil {
		k6common.Throw(rt, err)
	}
	return handle
}

// WaitForTimeout waits the specified amount of milliseconds
func (f *Frame) WaitForTimeout(timeout int64) {
	to := time.Duration(timeout) * time.Millisecond

	f.log.Debugf("Frame:WaitForTimeout", "tid:%s furl:%q to:%s", f.id, f.url, to)
	defer f.log.Debugf("Frame:WaitForTimeout:return", "tid:%s furl:%q to:%s", f.id, f.url, to)

	select {
	case <-f.ctx.Done():
	case <-time.After(to):
	}
}

func frameActionFn(f *Frame, selector string, state DOMElementState, strict bool, fn ElementHandleActionFn, states []string, force, noWaitAfter bool, timeout time.Duration) func(apiCtx context.Context, resultCh chan interface{}, errCh chan error) {
	// We execute a frame action in the following steps:
	// 1. Find element matching specified selector
	// 2. Wait for it to reach specified DOM state
	// 3. Run element handle action (incl. actionability checks)

	return func(apiCtx context.Context, resultCh chan interface{}, errCh chan error) {
		waitOpts := NewFrameWaitForSelectorOptions(f.defaultTimeout())
		waitOpts.State = state
		waitOpts.Strict = strict
		handle, err := f.waitForSelector(selector, waitOpts)
		if err != nil {
			errCh <- err
			return
		}
		if handle == nil {
			resultCh <- nil
			return
		}
		actFn := elementHandleActionFn(handle, states, fn, false, false, timeout)
		actFn(apiCtx, resultCh, errCh)
	}
}

func framePointerActionFn(f *Frame, selector string, state DOMElementState, strict bool, fn ElementHandlePointerActionFn, opts *ElementHandleBasePointerOptions) func(apiCtx context.Context, resultCh chan interface{}, errCh chan error) {
	// We execute a frame pointer action in the following steps:
	// 1. Find element matching specified selector
	// 2. Wait for it to reach specified DOM state
	// 3. Run element handle action (incl. actionability checks)

	return func(apiCtx context.Context, resultCh chan interface{}, errCh chan error) {
		waitOpts := NewFrameWaitForSelectorOptions(f.defaultTimeout())
		waitOpts.State = state
		waitOpts.Strict = strict
		handle, err := f.waitForSelector(selector, waitOpts)
		if err != nil {
			errCh <- err
			return
		}
		if handle == nil {
			resultCh <- nil
			return
		}
		pointerActFn := elementHandlePointerActionFn(handle, true, fn, opts)
		pointerActFn(apiCtx, resultCh, errCh)
	}
}
