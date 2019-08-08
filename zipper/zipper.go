package zipper

import (
	"context"
	"math"
	_ "net/http/pprof"
	"strings"
	"time"

	"github.com/go-graphite/carbonapi/zipper/broadcast"
	"github.com/go-graphite/carbonapi/zipper/config"
	"github.com/go-graphite/carbonapi/zipper/errors"
	"github.com/go-graphite/carbonapi/zipper/metadata"
	"github.com/go-graphite/carbonapi/zipper/types"
	protov2 "github.com/go-graphite/protocol/carbonapi_v2_pb"
	protov3 "github.com/go-graphite/protocol/carbonapi_v3_pb"
	"go.uber.org/zap"

	_ "github.com/go-graphite/carbonapi/zipper/protocols/auto"
	_ "github.com/go-graphite/carbonapi/zipper/protocols/graphite"
	_ "github.com/go-graphite/carbonapi/zipper/protocols/grpc"
	_ "github.com/go-graphite/carbonapi/zipper/protocols/prometheus"
	_ "github.com/go-graphite/carbonapi/zipper/protocols/v2"
	_ "github.com/go-graphite/carbonapi/zipper/protocols/v3"
)

// Zipper provides interface to Zipper-related functions
type Zipper struct {
	probeTicker *time.Ticker
	ProbeQuit   chan struct{}
	ProbeForce  chan int

	timeout           time.Duration
	timeoutConnect    time.Duration
	keepAliveInterval time.Duration

	searchConfigured bool
	searchBackends   types.BackendServer
	searchPrefix     string

	// Will broadcast to all servers there
	storeBackends             types.BackendServer
	concurrencyLimitPerServer int

	sendStats func(*types.Stats)

	logger *zap.Logger
}

var defaultTimeouts = types.Timeouts{
	Render:  10000 * time.Second,
	Find:    100 * time.Second,
	Connect: 200 * time.Millisecond,
}

func sanitizeTimouts(timeouts, defaultTimeouts types.Timeouts) types.Timeouts {
	if timeouts.Render == 0 {
		timeouts.Render = defaultTimeouts.Render
	}
	if timeouts.Find == 0 {
		timeouts.Find = defaultTimeouts.Find
	}

	if timeouts.Connect == 0 {
		timeouts.Connect = defaultTimeouts.Connect
	}

	return timeouts
}

func createBackendsV2(logger *zap.Logger, backends types.BackendsV2, expireDelaySec int32) ([]types.BackendServer, *errors.Errors) {
	storeClients := make([]types.BackendServer, 0)
	var e errors.Errors
	var ePtr *errors.Errors
	timeouts := backends.Timeouts
	for _, backend := range backends.Backends {
		concurrencyLimit := backends.ConcurrencyLimitPerServer
		tries := backends.MaxTries
		maxIdleConnsPerHost := backends.MaxIdleConnsPerHost
		keepAliveInterval := backends.KeepAliveInterval

		if backend.Timeouts == nil {
			backend.Timeouts = &timeouts
		}
		if backend.ConcurrencyLimit == nil {
			backend.ConcurrencyLimit = &concurrencyLimit
		}
		if backend.MaxTries == nil {
			backend.MaxTries = &tries
		}
		if backend.MaxIdleConnsPerHost == nil {
			backend.MaxIdleConnsPerHost = &maxIdleConnsPerHost
		}
		if backend.KeepAliveInterval == nil {
			backend.KeepAliveInterval = &keepAliveInterval
		}

		var client types.BackendServer
		logger.Debug("creating lb group",
			zap.String("name", backend.GroupName),
			zap.Strings("servers", backend.Servers),
			zap.Any("type", backend.LBMethod),
		)

		metadata.Metadata.RLock()
		backendInit, ok := metadata.Metadata.ProtocolInits[backend.Protocol]
		metadata.Metadata.RUnlock()
		if !ok {
			var protocols []string
			metadata.Metadata.RLock()
			for p := range metadata.Metadata.SupportedProtocols {
				protocols = append(protocols, p)
			}
			metadata.Metadata.RUnlock()
			logger.Error("unknown backend protocol",
				zap.Any("backend", backend),
				zap.String("requested_protocol", backend.Protocol),
				zap.Strings("supported_backends", protocols),
			)
			return nil, errors.Fatalf("unknown backend protocol '%v'", backend.Protocol)
		}

		var lbMethod types.LBMethod
		err := lbMethod.FromString(backend.LBMethod)
		if err != nil {
			logger.Fatal("failed to parse lbMethod",
				zap.String("lbMethod", backend.LBMethod),
				zap.Error(err),
			)
		}
		if lbMethod == types.RoundRobinLB {
			client, ePtr = backendInit(logger, backend)
			e.Merge(ePtr)
			if e.HaveFatalErrors {
				return nil, &e
			}
		} else {
			config := backend

			backends := make([]types.BackendServer, 0, len(backend.Servers))
			for _, server := range backend.Servers {
				config.Servers = []string{server}
				config.GroupName = server
				client, ePtr = backendInit(logger, config)
				e.Merge(ePtr)
				if e.HaveFatalErrors {
					return nil, &e
				}
				backends = append(backends, client)
			}

			client, ePtr = broadcast.NewBroadcastGroup(logger, backend.GroupName, backends, expireDelaySec, *backend.ConcurrencyLimit, backend.MaxBatchSize, timeouts)
			e.Merge(ePtr)
			if e.HaveFatalErrors {
				return nil, &e
			}
		}
		storeClients = append(storeClients, client)
	}
	return storeClients, nil
}

// NewZipper allows to create new Zipper
func NewZipper(sender func(*types.Stats), config *config.Config, logger *zap.Logger) (*Zipper, error) {
	config.Timeouts = sanitizeTimouts(config.Timeouts, defaultTimeouts)

	var searchBackends types.BackendServer
	var prefix string

	if config.InternalRoutingCache.Seconds() < 30 {
		logger.Warn("internalRoutingCache is too low",
			zap.String("reason", "this variable is used for internal routing cache, minimum allowed is 30s"),
			zap.String("recommendation", "it's usually good idea to set it to something like 600s"),
		)
		config.InternalRoutingCache = 60 * time.Second
	}

	// Convert old config format to new one
	if config.CarbonSearch.Backend != "" {
		config.CarbonSearchV2.BackendsV2 = types.BackendsV2{
			Backends: []types.BackendV2{{
				GroupName:           config.CarbonSearch.Backend,
				Protocol:            "carbonapi_v2_pb",
				LBMethod:            "roundrobin",
				Servers:             []string{config.CarbonSearch.Backend},
				Timeouts:            &config.Timeouts,
				ConcurrencyLimit:    &config.ConcurrencyLimitPerServer,
				KeepAliveInterval:   &config.KeepAliveInterval,
				MaxIdleConnsPerHost: &config.MaxIdleConnsPerHost,
				MaxTries:            &config.MaxTries,
			}},
			MaxIdleConnsPerHost:       config.MaxIdleConnsPerHost,
			ConcurrencyLimitPerServer: config.ConcurrencyLimitPerServer,
			Timeouts:                  config.Timeouts,
			KeepAliveInterval:         config.KeepAliveInterval,
			MaxTries:                  config.MaxTries,
		}
		config.CarbonSearchV2.Prefix = config.CarbonSearch.Prefix
	}

	if len(config.CarbonSearchV2.BackendsV2.Backends) > 0 {
		prefix = config.CarbonSearchV2.Prefix
		searchClients, err := createBackendsV2(logger, config.CarbonSearchV2.BackendsV2, int32(config.InternalRoutingCache.Seconds()))
		if err != nil && err.HaveFatalErrors {
			logger.Fatal("errors while initialing zipper search backends",
				zap.Any("errors", err.Errors),
			)
		}

		searchBackends, err = broadcast.NewBroadcastGroup(logger, "search", searchClients, int32(config.InternalRoutingCache.Seconds()), config.ConcurrencyLimitPerServer, config.MaxBatchSize, config.Timeouts)
		if err != nil && err.HaveFatalErrors {
			logger.Fatal("errors while initialing zipper search backends",
				zap.Any("errors", err.Errors),
			)
		}
	}

	// Convert old config format to new one
	if config.Backends != nil && len(config.Backends) != 0 {
		config.BackendsV2 = types.BackendsV2{
			Backends: []types.BackendV2{
				{
					GroupName:           "backends",
					Protocol:            "carbonapi_v2_pb",
					LBMethod:            "broadcast",
					Servers:             config.Backends,
					Timeouts:            &config.Timeouts,
					ConcurrencyLimit:    &config.ConcurrencyLimitPerServer,
					KeepAliveInterval:   &config.KeepAliveInterval,
					MaxIdleConnsPerHost: &config.MaxIdleConnsPerHost,
					MaxTries:            &config.MaxTries,
					MaxBatchSize:        config.MaxBatchSize,
				},
			},
			MaxIdleConnsPerHost:       config.MaxIdleConnsPerHost,
			ConcurrencyLimitPerServer: config.ConcurrencyLimitPerServer,
			Timeouts:                  config.Timeouts,
			KeepAliveInterval:         config.KeepAliveInterval,
			MaxTries:                  config.MaxTries,
			MaxBatchSize:              config.MaxBatchSize,
		}
	}

	config.BackendsV2.Timeouts = sanitizeTimouts(config.BackendsV2.Timeouts, config.Timeouts)
	for i := range config.BackendsV2.Backends {
		if config.BackendsV2.Backends[i].Timeouts == nil {
			timeouts := config.BackendsV2.Timeouts
			config.BackendsV2.Backends[i].Timeouts = &timeouts
		}
		timeouts := sanitizeTimouts(*(config.BackendsV2.Backends[i].Timeouts), config.BackendsV2.Timeouts)
		config.BackendsV2.Backends[i].Timeouts = &timeouts
	}

	storeClients, err := createBackendsV2(logger, config.BackendsV2, int32(config.InternalRoutingCache.Seconds()))
	if err != nil && err.HaveFatalErrors {
		logger.Fatal("errors while initialing zipper store backends",
			zap.Any("errors", err.Errors),
		)
	}

	var storeBackends types.BackendServer
	storeBackends, err = broadcast.NewBroadcastGroup(logger, "root", storeClients, int32(config.InternalRoutingCache.Seconds()), config.ConcurrencyLimitPerServer, config.MaxBatchSize, config.Timeouts)
	if err != nil && err.HaveFatalErrors {
		logger.Fatal("errors while initialing zipper store backends",
			zap.Any("errors", err.Errors),
		)
	}

	z := &Zipper{
		probeTicker: time.NewTicker(config.InternalRoutingCache),
		ProbeQuit:   make(chan struct{}),
		ProbeForce:  make(chan int),

		sendStats: sender,

		storeBackends:             storeBackends,
		searchBackends:            searchBackends,
		searchPrefix:              prefix,
		searchConfigured:          len(prefix) > 0 && len(searchBackends.Backends()) > 0,
		concurrencyLimitPerServer: config.ConcurrencyLimitPerServer,
		keepAliveInterval:         config.KeepAliveInterval,
		timeout:                   config.Timeouts.Render,
		timeoutConnect:            config.Timeouts.Connect,
		logger:                    logger,
	}

	logger.Debug("zipper config",
		zap.Any("config", config),
	)

	go z.probeTlds()

	z.ProbeForce <- 1
	return z, nil
}

func (z *Zipper) doProbe(logger *zap.Logger) {
	ctx := context.Background()

	_, err := z.storeBackends.ProbeTLDs(ctx)
	if err != nil && err.HaveFatalErrors {
		logger.Error("failed to probe tlds",
			zap.Any("errors", err.Errors),
		)
	}
}

func (z *Zipper) probeTlds() {
	logger := z.logger.With(zap.String("type", "probe"))
	for {
		select {
		case <-z.probeTicker.C:
			z.doProbe(logger)
		case <-z.ProbeForce:
			z.doProbe(logger)
		case <-z.ProbeQuit:
			z.probeTicker.Stop()
			return
		}
	}
}

// GRPC-compatible methods
func (z Zipper) FetchProtoV3(ctx context.Context, request *protov3.MultiFetchRequest) (*protov3.MultiFetchResponse, *types.Stats, error) {
	var statsSearch *types.Stats
	var e errors.Errors

	if z.searchConfigured {
		realRequest := &protov3.MultiFetchRequest{
			Metrics: make([]protov3.FetchRequest, 0, len(request.Metrics)),
		}

		for _, metric := range request.Metrics {
			if strings.HasPrefix(metric.Name, z.searchPrefix) {
				res, stat, err := z.searchBackends.Find(ctx, &protov3.MultiGlobRequest{
					Metrics: []string{metric.Name},
				})

				if statsSearch == nil {
					statsSearch = stat
				} else {
					statsSearch.Merge(stat)
				}

				if err != nil {
					e.Merge(err)
					continue
				}

				if len(res.Metrics) == 0 {
					continue
				}

				metricRequests := make([]protov3.FetchRequest, 0, len(res.Metrics))
				for _, n := range res.Metrics {
					for _, m := range n.Matches {
						metricRequests = append(metricRequests, protov3.FetchRequest{
							Name:            m.Path,
							StartTime:       metric.StartTime,
							StopTime:        metric.StopTime,
							FilterFunctions: metric.FilterFunctions,
						})
					}
				}

				if len(metricRequests) > 0 {
					realRequest.Metrics = append(realRequest.Metrics, metricRequests...)
				}

			} else {
				realRequest.Metrics = append(realRequest.Metrics, metric)
			}
		}

		if len(realRequest.Metrics) > 0 {
			request = realRequest
		}
	}

	res, stats, err := z.storeBackends.Fetch(ctx, request)
	if statsSearch != nil {
		if stats == nil {
			stats = statsSearch
		} else {
			stats.Merge(statsSearch)
		}
	}

	e.Merge(err)

	if e.HaveFatalErrors || res == nil {
		z.logger.Error("had fatal errors while fetching result",
			zap.Any("errors", e.Errors),
		)
		return nil, nil, types.ErrNoMetricsFetched
	}

	return res, stats, nil
}

func (z Zipper) FindProtoV3(ctx context.Context, request *protov3.MultiGlobRequest) (*protov3.MultiGlobResponse, *types.Stats, error) {
	searchRequests := &protov3.MultiGlobRequest{}
	if z.searchConfigured {
		realRequest := &protov3.MultiGlobRequest{Metrics: make([]string, 0, len(request.Metrics)), StartTime: request.StartTime, StopTime: request.StopTime}
		for _, m := range request.Metrics {
			if strings.HasPrefix(m, z.searchPrefix) {
				searchRequests.Metrics = append(searchRequests.Metrics, m)
			} else {
				realRequest.Metrics = append(realRequest.Metrics, m)
			}
		}
		if len(searchRequests.Metrics) > 0 {
			request = realRequest
		}
	}

	res, stats, err := z.storeBackends.Find(ctx, request)
	if err == nil {
		err = &errors.Errors{}
	}

	findResponse := &types.ServerFindResponse{
		Response: res,
		Stats:    stats,
		Err:      err,
	}

	if len(searchRequests.Metrics) > 0 {
		resSearch, statsSearch, err := z.searchBackends.Find(ctx, request)
		searchResponse := &types.ServerFindResponse{
			Response: resSearch,
			Stats:    statsSearch,
			Err:      err,
		}
		findResponse.Merge(searchResponse)
	}

	if findResponse.Err.HaveFatalErrors {
		z.logger.Error("had fatal errors during request",
			zap.Any("errors", findResponse.Err.Errors),
		)
		return nil, nil, types.ErrNoMetricsFetched
	} else if len(findResponse.Err.Errors) > 0 {
		z.logger.Warn("got non-fatal errors during request",
			zap.Any("errors", findResponse.Err.Errors),
		)
	}

	return findResponse.Response, findResponse.Stats, nil
}

func (z Zipper) InfoProtoV3(ctx context.Context, request *protov3.MultiGlobRequest) (*protov3.ZipperInfoResponse, *types.Stats, error) {
	realRequest := &protov3.MultiMetricsInfoRequest{Names: make([]string, 0, len(request.Metrics))}
	res, _, err := z.FindProtoV3(ctx, request)
	if err == nil || err == types.ErrNonFatalErrors {
		for _, m := range res.Metrics {
			for _, match := range m.Matches {
				if match.IsLeaf {
					realRequest.Names = append(realRequest.Names, match.Path)
				}
			}
		}
	} else {
		realRequest.Names = append(realRequest.Names, request.Metrics...)
	}

	r, stats, e := z.storeBackends.Info(ctx, realRequest)
	if e.HaveFatalErrors {
		z.logger.Error("had fatal errors during request",
			zap.Any("errors", e.Errors),
		)
		return nil, nil, types.ErrNoMetricsFetched
	} else if len(e.Errors) > 0 {
		z.logger.Warn("got non-fatal errors during request",
			zap.Any("errors", e.Errors),
		)
	}

	return r, stats, nil
}

func (z Zipper) ListProtoV3(ctx context.Context) (*protov3.ListMetricsResponse, *types.Stats, error) {
	r, stats, e := z.storeBackends.List(ctx)
	if e.HaveFatalErrors {
		z.logger.Error("had fatal errors during request",
			zap.Any("errors", e.Errors),
		)
		return nil, nil, types.ErrNoMetricsFetched
	} else if len(e.Errors) > 0 {
		z.logger.Warn("got non-fatal errors during request",
			zap.Any("errors", e.Errors),
		)
	}

	return r, stats, nil
}
func (z Zipper) StatsProtoV3(ctx context.Context) (*protov3.MetricDetailsResponse, *types.Stats, error) {
	r, stats, e := z.storeBackends.Stats(ctx)
	if e.HaveFatalErrors {
		z.logger.Error("had fatal errors while fetching result",
			zap.Any("errors", e.Errors),
		)
		return nil, stats, types.ErrNoMetricsFetched
	}

	return r, stats, nil
}

// PB3-compatible methods
func (z Zipper) FetchProtoV2(ctx context.Context, query []string, startTime, stopTime int32) (*protov2.MultiFetchResponse, *types.Stats, error) {
	request := &protov3.MultiFetchRequest{}
	for _, q := range query {
		request.Metrics = append(request.Metrics, protov3.FetchRequest{
			Name:      q,
			StartTime: int64(startTime),
			StopTime:  int64(stopTime),
		})
	}

	grpcRes, stats, err := z.FetchProtoV3(ctx, request)
	if err != nil {
		return nil, nil, err
	}

	var res protov2.MultiFetchResponse
	for i := range grpcRes.Metrics {
		vals := make([]float64, 0, len(grpcRes.Metrics[i].Values))
		isAbsent := make([]bool, 0, len(grpcRes.Metrics[i].Values))
		for _, v := range grpcRes.Metrics[i].Values {
			if math.IsNaN(v) {
				vals = append(vals, 0)
				isAbsent = append(isAbsent, true)
			} else {
				vals = append(vals, v)
				isAbsent = append(isAbsent, false)
			}
		}
		res.Metrics = append(res.Metrics,
			protov2.FetchResponse{
				Name:      grpcRes.Metrics[i].Name,
				StartTime: int32(grpcRes.Metrics[i].StartTime),
				StopTime:  int32(grpcRes.Metrics[i].StopTime),
				StepTime:  int32(grpcRes.Metrics[i].StepTime),
				Values:    vals,
				IsAbsent:  isAbsent,
			})
	}

	return &res, stats, nil
}

func (z Zipper) FindProtoV2(ctx context.Context, query []string, startTime, stopTime int32) ([]*protov2.GlobResponse, *types.Stats, error) {
	request := &protov3.MultiGlobRequest{
		Metrics:   query,
		StartTime: int64(startTime),
		StopTime:  int64(stopTime),
	}
	grpcReses, stats, err := z.FindProtoV3(ctx, request)
	if err != nil {
		return nil, nil, err
	}

	reses := make([]*protov2.GlobResponse, 0, len(grpcReses.Metrics))
	for _, grpcRes := range grpcReses.Metrics {

		res := &protov2.GlobResponse{
			Name: grpcRes.Name,
		}

		for _, v := range grpcRes.Matches {
			match := protov2.GlobMatch{
				Path:   v.Path,
				IsLeaf: v.IsLeaf,
			}
			res.Matches = append(res.Matches, match)
		}
		reses = append(reses, res)
	}

	return reses, stats, nil
}

func (z Zipper) InfoProtoV2(ctx context.Context, targets []string) (*protov2.ZipperInfoResponse, *types.Stats, error) {
	request := &protov3.MultiGlobRequest{
		Metrics: targets,
	}
	grpcRes, stats, err := z.InfoProtoV3(ctx, request)
	if err != nil {
		return nil, nil, err
	}

	res := &protov2.ZipperInfoResponse{}

	for k, i := range grpcRes.Info {
		for _, v := range i.Metrics {
			rets := make([]protov2.Retention, 0, len(v.Retentions))
			for _, ret := range v.Retentions {
				rets = append(rets, protov2.Retention{
					SecondsPerPoint: int32(ret.SecondsPerPoint),
					NumberOfPoints:  int32(ret.NumberOfPoints),
				})
			}
			i := &protov2.InfoResponse{
				Name:              v.Name,
				AggregationMethod: v.ConsolidationFunc,
				MaxRetention:      int32(v.MaxRetention),
				XFilesFactor:      v.XFilesFactor,
				Retentions:        rets,
			}
			res.Responses = append(res.Responses, protov2.ServerInfoResponse{
				Server: k,
				Info:   i,
			})
		}
	}

	return res, stats, nil
}
func (z Zipper) ListProtoV2(ctx context.Context) (*protov2.ListMetricsResponse, *types.Stats, error) {
	grpcRes, stats, err := z.ListProtoV3(ctx)
	if err != nil {
		return nil, nil, err
	}

	res := &protov2.ListMetricsResponse{
		Metrics: grpcRes.Metrics,
	}
	return res, stats, nil
}
func (z Zipper) StatsProtoV2(ctx context.Context) (*protov2.MetricDetailsResponse, *types.Stats, error) {
	grpcRes, stats, err := z.StatsProtoV3(ctx)
	if err != nil {
		return nil, nil, err
	}

	metrics := make(map[string]*protov2.MetricDetails, len(grpcRes.Metrics))
	for k, v := range grpcRes.Metrics {
		metrics[k] = &protov2.MetricDetails{
			Size_:   v.Size_,
			ModTime: v.ModTime,
			ATime:   v.ATime,
			RdTime:  v.RdTime,
		}
	}

	res := &protov2.MetricDetailsResponse{
		FreeSpace:  grpcRes.FreeSpace,
		TotalSpace: grpcRes.TotalSpace,
		Metrics:    metrics,
	}

	return res, stats, nil
}

// Tags

func (z Zipper) TagNames(ctx context.Context, query string, limit int64) ([]string, error) {
	data, e := z.storeBackends.TagNames(ctx, query, limit)
	if e.HaveFatalErrors {
		z.logger.Error("had fatal errors during request",
			zap.Any("errors", e.Errors),
		)

		return data, types.ErrNoMetricsFetched
	}

	if len(e.Errors) > 0 {
		z.logger.Warn("got non-fatal errors during request",
			zap.Any("errors", e.Errors),
		)
	}

	return data, nil
}

func (z Zipper) TagValues(ctx context.Context, query string, limit int64) ([]string, error) {
	data, e := z.storeBackends.TagValues(ctx, query, limit)
	if e.HaveFatalErrors {
		z.logger.Error("had fatal errors during request",
			zap.Any("errors", e.Errors),
		)
		return data, types.ErrNoMetricsFetched
	}

	if len(e.Errors) > 0 {
		z.logger.Warn("got non-fatal errors during request",
			zap.Any("errors", e.Errors),
		)
	}

	return data, nil
}
