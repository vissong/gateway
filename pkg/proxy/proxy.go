package proxy

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fagongzi/gateway/pkg/filter"
	"github.com/fagongzi/gateway/pkg/pb/metapb"
	"github.com/fagongzi/gateway/pkg/store"
	"github.com/fagongzi/gateway/pkg/util"
	"github.com/fagongzi/log"
	"github.com/fagongzi/util/hack"
	"github.com/fagongzi/util/task"
	"github.com/soheilhy/cmux"
	"github.com/valyala/fasthttp"
)

var (
	// ErrPrefixRequestCancel user cancel request error
	ErrPrefixRequestCancel = "request canceled"
	// ErrNoServer no server
	ErrNoServer = errors.New("has no server")
	// ErrRewriteNotMatch rewrite not match request url
	ErrRewriteNotMatch = errors.New("rewrite not match request url")
)


// 合并请求的时候，使用同一的 ContentType
// 同时需要去掉一些 header 
var (
	// MultiResultsContentType merge operation using content-type
	MultiResultsContentType = "application/json; charset=utf-8"
	// MultiResultsRemoveHeaders merge operation need to remove headers
	MultiResultsRemoveHeaders = []string{
		"Content-Length",
		"Content-Type",
		"Date",
	}
)

var (
	globalHTTPOptions *util.HTTPOption
)

const (
	charLeft  = byte('[')
	charRight = byte(']')
)

// Proxy Proxy
type Proxy struct {
	// 这里有一个索，需要关注是做什么
	sync.RWMutex

	dispatchIndex, copyIndex uint64
	dispatches               []chan *dispathNode
	copies                   []chan *copyReq

	cfg        *Cfg
	filtersMap map[string]filter.Filter
	filters    []filter.Filter
	client     *util.FastHTTPClient
	dispatcher *dispatcher

	rpcListener net.Listener

	runner   *task.Runner
	stopped  int32
	stopC    chan struct{}
	stopOnce sync.Once
	stopWG   sync.WaitGroup
}

// NewProxy create a new proxy
func NewProxy(cfg *Cfg) *Proxy {
	globalHTTPOptions = &util.HTTPOption{
		MaxConnDuration:     cfg.Option.LimitDurationConnKeepalive,
		MaxIdleConnDuration: cfg.Option.LimitDurationConnIdle,
		ReadTimeout:         cfg.Option.LimitTimeoutRead,
		WriteTimeout:        cfg.Option.LimitTimeoutWrite,
		MaxResponseBodySize: cfg.Option.LimitBytesBody,
		WriteBufferSize:     cfg.Option.LimitBufferWrite,
		ReadBufferSize:      cfg.Option.LimitBufferRead,
		MaxConns:            cfg.Option.LimitCountConn,
	}

	p := &Proxy{
		client:        util.NewFastHTTPClientOption(globalHTTPOptions),
		cfg:           cfg,
		filtersMap:    make(map[string]filter.Filter),
		stopC:         make(chan struct{}),
		runner:        task.NewRunner(),
		copies:        make([]chan *copyReq, cfg.Option.LimitCountCopyWorker, cfg.Option.LimitCountCopyWorker),
		dispatches:    make([]chan *dispathNode, cfg.Option.LimitCountDispatchWorker, cfg.Option.LimitCountDispatchWorker),
		dispatchIndex: 0,
		copyIndex:     0,
	}

	p.init()

	return p
}

// Start start proxy
func (p *Proxy) Start() {
	// 起换一个 goroutine 监听停止指令
	go p.listenToStop()

	// TODO
	util.StartMetricsPush(p.runner, p.cfg.Metric)

	// 创建 copy 与 dispatch 的 worker
	p.readyToCopy()
	p.readyToDispatch()

	log.Infof("gateway proxy started at <%s>", p.cfg.Addr)

	// 没有启用 websocket 的情况下，直接监听 http server
	// 但是这里感觉应该专门判断下开启了 ws 做特殊处理厚一些
	if !p.cfg.Option.EnableWebSocket {
		err := fasthttp.ListenAndServe(p.cfg.Addr, p.ServeFastHTTP)
		if err != nil {
			log.Fatalf("gateway proxy start failed, errors:\n%+v",
				err)
		}
		return
	}

	// 以下流程是对 ws 协议的处理流程，直接监听在 tcp 协议上。
	l, err := net.Listen("tcp", p.cfg.Addr)
	if err != nil {
		log.Fatalf("gateway proxy start failed, errors:\n%+v",
			err)
	}

	m := cmux.New(l)
	webSocketL := m.Match(cmux.HTTP1HeaderField("Upgrade", "websocket"))
	httpL := m.Match(cmux.Any())

	// http1 用于升级请求
	go func() {
		httpS := fasthttp.Server{
			Handler: p.ServeFastHTTP,
		}
		err = httpS.Serve(httpL)
		if err != nil {
			log.Fatalf("gateway proxy start failed, errors:\n%+v",
				err)
		}
	}()

	// ws 用于维持请求
	go func() {
		webSocketS := &http.Server{
			Handler: p,
		}
		err = webSocketS.Serve(webSocketL)
		if err != nil {
			log.Fatalf("gateway proxy start failed, errors:\n%+v",
				err)
		}
	}()

	err = m.Serve()
	if err != nil {
		log.Fatalf("gateway proxy start failed, errors:\n%+v",
			err)
	}
}

// Stop stop the proxy
func (p *Proxy) Stop() {
	log.Infof("stop: start to stop gateway proxy")

	p.stopWG.Add(1)
	p.stopC <- struct{}{}
	p.stopWG.Wait()

	log.Infof("stop: gateway proxy stopped")
}

// 监听停止的 chan，如果收到，则，停止 proxy
func (p *Proxy) listenToStop() {
	<-p.stopC
	p.doStop()
}

func (p *Proxy) doStop() {
	p.stopOnce.Do(func() {
		defer p.stopWG.Done()
		p.setStopped()
		p.runner.Stop()
	})
}

func (p *Proxy) stopRPC() error {
	return p.rpcListener.Close()
}

func (p *Proxy) setStopped() {
	// 停止标记
	atomic.StoreInt32(&p.stopped, 1)
}

func (p *Proxy) isStopped() bool {
	return atomic.LoadInt32(&p.stopped) == 1
}

/*
1. initDispatcher 初始化后端分发路由表
2. initFilters 初始化插件
3. 注册代理
4. 加载路由表 ddispatcher.load 
*/
func (p *Proxy) init() {
	err := p.initDispatcher()
	if err != nil {
		log.Fatalf("init route table failed, errors:\n%+v",
			err)
	}

	p.initFilters()

	err = p.dispatcher.store.RegistryProxy(&metapb.Proxy{
		Addr:    p.cfg.Addr,
		AddrRPC: p.cfg.AddrRPC,
	}, p.cfg.TTLProxy)
	if err != nil {
		log.Fatalf("init route table failed, errors:\n%+v",
			err)
	}

	// 加载了很多的数据，可以去到详细定义去查看
	p.dispatcher.load()
}

/* 
从存储中读取后端地址，并对 dispatcher 进行初始化
 */
func (p *Proxy) initDispatcher() error {
	s, err := store.GetStoreFrom(p.cfg.AddrStore, p.cfg.Namespace, p.cfg.AddrStoreUserName, p.cfg.AddrStorePwd)

	if err != nil {
		return err
	}

	p.dispatcher = newDispatcher(p.cfg, s, p.runner)
	return nil
}

/* 
加载创建，并执行插件的 init 方法，都成功后，添加到插件列表中
*/
func (p *Proxy) initFilters() {
	for _, filter := range p.cfg.Filers {
		f, err := p.newFilter(filter)
		if nil != err {
			log.Fatalf("init filter failed, filter=<%+v> errors:\n%+v",
				filter,
				err)
		}

		err = f.Init(filter.ExternalCfg)
		if nil != err {
			log.Fatalf("init filter failed, filter=<%+v> errors:\n%+v",
				filter,
				err)
		}

		log.Infof("filter added, filter=<%+v>", filter)
		p.filters = append(p.filters, f)
		p.filtersMap[f.Name()] = f
	}
}

/* 
叫 readyToDispatch 更合适
根据配置，创建 dispatch req chan，队列长度为 1024 个
*/
func (p *Proxy) readyToDispatch() {
	for i := uint64(0); i < p.cfg.Option.LimitCountDispatchWorker; i++ {
		c := make(chan *dispathNode, 1024)
		p.dispatches[i] = c

		_, err := p.runner.RunCancelableTask(func(ctx context.Context) {
			for {
				select {
				case <-ctx.Done():
					return
				case dn := <-c:
					if dn != nil {
						p.doProxy(dn, nil)
					}
				}
			}
		})
		if err != nil {
			log.Fatalf("init dispatch workers failed, errors:\n%+v", err)
		}
	}
}

/* 
叫 initCopyWorker 更合适
根据配置，创建 copy req chan，队列长度为 1024 个
*/
func (p *Proxy) readyToCopy() {
	for i := uint64(0); i < p.cfg.Option.LimitCountCopyWorker; i++ {
		c := make(chan *copyReq, 1024)
		p.copies[i] = c

		_, err := p.runner.RunCancelableTask(func(ctx context.Context) {
			for {
				select {
				case <-ctx.Done():
					return
				case req := <-c:
					if req != nil {
						p.doCopy(req)
					}
				}
			}
		})
		if err != nil {
			log.Fatalf("init copy workers failed, errors:\n%+v", err)
		}
	}
}

// ServeFastHTTP http reverse handler by fasthttp
func (p *Proxy) ServeFastHTTP(ctx *fasthttp.RequestCtx) {
	var buf bytes.Buffer
	buf.WriteByte(charLeft)
	buf.Write(ctx.Method())
	buf.WriteByte(charRight)
	buf.Write(ctx.RequestURI())
	requestTag := hack.SliceToString(buf.Bytes())

	if p.isStopped() {
		log.Infof("proxy is stopped")
		ctx.SetStatusCode(fasthttp.StatusServiceUnavailable)
		return
	}

	startAt := time.Now()
	api, dispatches := p.dispatcher.dispatch(&ctx.Request, requestTag)
	if len(dispatches) == 0 &&
		(nil == api || api.meta.DefaultValue == nil) {
		ctx.SetStatusCode(fasthttp.StatusNotFound)
		p.dispatcher.dispatchCompleted()

		log.Infof("%s: not match, return with 404",
			requestTag)
		return
	}

	// make sure fasthttp request header is parsed, avoid concurrent copy header bug
	ctx.Request.Header.Peek("fuck")

	log.Infof("%s: match api %s, has %d dispatches",
		requestTag,
		api.meta.Name,
		len(dispatches))

	incrRequest(api.meta.Name)

	rd := acquireRender()
	rd.init(requestTag, api, dispatches)

	var multiCtx *multiContext
	var wg *sync.WaitGroup
	lastBatch := int32(0)
	num := len(dispatches)

	if num > 1 {
		wg = acquireWG()
		multiCtx = acquireMultiContext()
		multiCtx.init()
	}

	for idx, dn := range dispatches {
		// wait last batch complete
		if wg != nil && lastBatch < dn.node.meta.BatchIndex {
			wg.Wait()
			wg = nil
			lastBatch = dn.node.meta.BatchIndex
			if num-idx > 1 {
				wg = &sync.WaitGroup{}
			}
		}

		if wg != nil {
			dn.wg = wg
			wg.Add(1)
		}

		dn.multiCtx = multiCtx
		dn.requestTag = requestTag
		dn.rd = rd
		dn.ctx = ctx
		if dn.copyTo != nil {
			log.Infof("%s: dipatch node %d copy to %s",
				requestTag,
				idx,
				dn.copyTo.meta.Addr)

			p.copies[getIndex(&p.copyIndex, p.cfg.Option.LimitCountCopyWorker)] <- &copyReq{
				origin:     copyRequest(&ctx.Request),
				to:         dn.copyTo.clone(),
				api:        dn.api.clone(),
				node:       dn.node.clone(),
				idx:        idx,
				requestTag: requestTag,
			}
		}

		if wg != nil {
			p.dispatches[getIndex(&p.dispatchIndex, p.cfg.Option.LimitCountDispatchWorker)] <- dn
		} else {
			p.doProxy(dn, nil)
		}
	}

	// wait last batch complete
	if wg != nil {
		wg.Wait()
		releaseWG(wg)
	}

	rd.render(ctx, multiCtx)
	releaseRender(rd)
	releaseMultiContext(multiCtx)

	p.postRequest(api, dispatches, startAt)
	p.dispatcher.dispatchCompleted()

	log.Debugf("%s: dispatch complete",
		requestTag)
}

func (p *Proxy) doCopy(req *copyReq) {
	svr := req.to

	if nil == svr {
		return
	}

	req.prepare()

	log.Infof("%s: dipatch node %d copy to %s",
		req.requestTag,
		req.idx,
		req.to.meta.Addr)

	res, err := p.client.Do(req.origin, svr.meta.Addr, nil)
	if err != nil {
		log.Errorf("%s: dipatch node %d copy to %s with error %s",
			req.requestTag,
			req.idx,
			req.to.meta.Addr,
			err)
		fasthttp.ReleaseRequest(req.origin)
		return
	}

	if res != nil {
		fasthttp.ReleaseResponse(res)
	}

	fasthttp.ReleaseRequest(req.origin)
}

func (p *Proxy) doProxy(dn *dispathNode, adjustH func(*proxyContext)) {
	if dn.node.meta.UseDefault {
		dn.maybeDone()
		log.Infof("%s: dipatch node %d force using default",
			dn.requestTag,
			dn.idx)
		return
	}

	// req上下文
	ctx := dn.ctx
	// runtime
	svr := dn.dest
	if nil == svr {
		dn.err = ErrNoServer
		dn.code = fasthttp.StatusServiceUnavailable
		dn.maybeDone()
		log.Infof("%s: dipatch node %d has no server, return with 503",
			dn.requestTag,
			dn.idx)
		return
	}

	// 复制原始请求
	forwardReq := copyRequest(&ctx.Request)

	// change url 是否需要重写
	if dn.needRewrite() {
		// if not use rewrite, it only change uri path and query string
		realPath := dn.rewiteURL(&ctx.Request)
		if "" != realPath {
			log.Infof("%s: dipatch node %d rewrite url to %s",
				dn.requestTag,
				dn.idx,
				realPath)

			forwardReq.SetRequestURI(realPath)
		} else {
			dn.err = ErrRewriteNotMatch
			dn.code = fasthttp.StatusBadRequest
			dn.maybeDone()

			log.Warnf("%s: dipatch node %d rewrite not match, return with 400",
				dn.requestTag,
				dn.idx)
			return
		}
	}

	// 获取新的上下文，如果原始上下文中有，则copy
	c := acquireContext()
	c.init(p.dispatcher, ctx, forwardReq, dn)
	if adjustH != nil {
		adjustH(c)
	}

	// pre filters
	filterName, code, err := p.doPreFilters(c)
	if nil != err {
		dn.err = err
		dn.code = code
		dn.maybeDone()
		releaseContext(c)

		log.Errorf("%s: dipatch node %d call filter %s pre failed with error %s",
			dn.requestTag,
			dn.idx,
			filterName,
			err)
		return
	}

	// hit cache
	if value := c.GetAttr(filter.UsingCachingValue); nil != value {
		dn.cachedCT, dn.cachedBody = filter.ParseCachedValue(value.([]byte))
		dn.maybeDone()
		releaseContext(c)

		log.Infof("%s: dipatch node %d using cache",
			dn.requestTag,
			dn.idx)
		return
	}

	var res *fasthttp.Response
	times := int32(0)
	for {
		log.Infof("%s: dipatch node %d sent for %d times",
			dn.requestTag,
			dn.idx,
			times)

		if !dn.api.isWebSocket() {
			forwardReq.SetHost(svr.meta.Addr)
			res, err = p.client.Do(forwardReq, svr.meta.Addr, dn.httpOption())
		} else {
			res, err = p.onWebsocket(c, svr.meta.Addr)
		}
		c.setEndAt(time.Now())

		times++

		// skip succeed 成功的，流程结束
		if err == nil && res.StatusCode() < fasthttp.StatusBadRequest {
			break
		}

		// skip no retry strategy 没有重试策略的，直接结束
		if !dn.hasRetryStrategy() {
			break
		}

		// skip not match 重试策略不匹配
		if !dn.matchAllRetryStrategy() &&
			!dn.matchRetryStrategy(int32(res.StatusCode())) {
			break
		}

		// retry with strategiess 重试次数超过设置
		retry := dn.retryStrategy()
		if times >= retry.MaxTimes {
			log.Infof("%s: dipatch node %d sent times over the max %d",
				dn.requestTag,
				dn.idx,
				retry.MaxTimes)
			break
		}

		if retry.Interval > 0 {
			time.Sleep(time.Millisecond * time.Duration(retry.Interval))
		}

		fasthttp.ReleaseResponse(res)
		// 选择一个新的server发送请求
		p.dispatcher.selectServer(&ctx.Request, dn, dn.requestTag)
		svr = dn.dest
		if nil == svr {
			dn.err = ErrNoServer
			dn.code = fasthttp.StatusServiceUnavailable
			dn.maybeDone()

			log.Infof("%s: dipatch node %d has no server, return with 503",
				dn.requestTag,
				dn.idx)
			return
		}
	}

	dn.res = res
	if err != nil || res.StatusCode() >= fasthttp.StatusBadRequest {
		resCode := fasthttp.StatusInternalServerError

		if nil != err {
			log.Errorf("%s: dipatch node %d failed with error %s",
				dn.requestTag,
				dn.idx,
				err)
		} else {
			resCode = res.StatusCode()
			log.Errorf("%s: dipatch node %d failed with error code %d",
				dn.requestTag,
				dn.idx,
				resCode)
		}

		if nil == err || !strings.HasPrefix(err.Error(), ErrPrefixRequestCancel) {
			p.doPostErrFilters(c)
		}

		dn.err = err
		dn.code = resCode
		dn.maybeDone()
		releaseContext(c)
		return
	}

	if log.DebugEnabled() {
		log.Debugf("%s: dipatch node %d return by %s with code %d, body <%s>",
			dn.requestTag,
			dn.idx,
			svr.meta.Addr,
			res.StatusCode(),
			hack.SliceToString(res.Body()))
	}

	// post filters
	filterName, code, err = p.doPostFilters(c)
	if nil != err {
		log.Errorf("%s: dipatch node %d call filter %s post failed with error %s",
			dn.requestTag,
			dn.idx,
			filterName,
			err)

		dn.err = err
		dn.code = code
		dn.maybeDone()
		releaseContext(c)
		return
	}

	dn.maybeDone()
	releaseContext(c)
}

func getIndex(opt *uint64, size uint64) int {
	return int(atomic.AddUint64(opt, 1) % size)
}
