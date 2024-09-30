package router

import (
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/BaritoLog/barito-router/appcontext"
	"github.com/BaritoLog/barito-router/config"
	"github.com/BaritoLog/barito-router/instrumentation"
	"github.com/BaritoLog/go-boilerplate/httpkit"
	"github.com/gorilla/mux"
	"github.com/hashicorp/consul/api"
	"github.com/opentracing/opentracing-go"
	"github.com/patrickmn/go-cache"
	"golang.org/x/time/rate"
)

const (
	// KeyKibana is meta service name of kibana
	KeyKibana = "kibana"

	// AppKibanaNoProfilePath is path to register when server returned no profile
	AppKibanaNoProfilePath = "api/kibana_no_profile"
)

type KibanaRouter interface {
	ServeHTTP(w http.ResponseWriter, req *http.Request)
	MustBeAuthorizedMiddleware(http.Handler) http.Handler
	ServeElasticsearch(w http.ResponseWriter, req *http.Request)
}

type kibanaRouter struct {
	addr          string
	marketUrl     string
	accessToken   string
	profilePath   string
	authorizePath string
	ssoEnabled    bool
	ssoClient     SSOClient

	cacheBag *cache.Cache

	client *http.Client
	appCtx *appcontext.AppContext

	limiter *rate.Limiter
}

// NewKibanaRouter is a function for creating new kibana router
func NewKibanaRouter(addr, marketUrl, accessToken, profilePath, authorizePath string, appCtx *appcontext.AppContext) KibanaRouter {
	return &kibanaRouter{
		addr:          addr,
		marketUrl:     marketUrl,
		accessToken:   accessToken,
		profilePath:   profilePath,
		authorizePath: authorizePath,
		cacheBag:      cache.New(config.CacheExpirationTimeSeconds, 2*config.CacheExpirationTimeSeconds),
		client:        createClient(),
		appCtx:        appCtx,
		limiter:       rate.NewLimiter(rate.Every(time.Second), 1),
	}
}

func NewKibanaRouterWithSSO(addr, marketUrl, accessToken, profilePath, authorizePath string, appCtx *appcontext.AppContext, ssoClient SSOClient) KibanaRouter {
	return &kibanaRouter{
		addr:          addr,
		marketUrl:     marketUrl,
		accessToken:   accessToken,
		profilePath:   profilePath,
		authorizePath: authorizePath,
		cacheBag:      cache.New(config.CacheExpirationTimeSeconds, 2*config.CacheExpirationTimeSeconds),
		client:        createClient(),
		appCtx:        appCtx,
		ssoClient:     ssoClient,
		ssoEnabled:    true,
		limiter:       rate.NewLimiter(rate.Every(time.Second), 1),
	}
}

func (r *kibanaRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {

	if strings.HasPrefix(req.URL.Path, "/elasticsearch") {
		r.ServeElasticsearch(w, req)
		return
	}

	if req.URL.Path == "/ping" {
		OnPing(w, req)
		return
	}

	span := opentracing.StartSpan("barito_router_viewer.view_kibana")
	defer span.Finish()

	params := mux.Vars(req)
	clusterName := params["cluster_name"]

	span.SetTag("app-group", clusterName)
	profile, err := fetchProfileByClusterName(r.client, span.Context(), r.cacheBag, r.marketUrl, r.accessToken, r.profilePath, clusterName)
	if profile != nil {
		instrumentation.RunTransaction(r.appCtx.NewRelicApp(), r.profilePath, w, req)
	}
	if err != nil {
		onTradeError(w, err)
		return
	}

	if profile == nil {
		onNoProfile(w)
		instrumentation.RunTransaction(r.appCtx.NewRelicApp(), AppKibanaNoProfilePath, w, req)
		return
	}

	sourceUrl := fmt.Sprintf("%s://%s:%s", httpkit.SchemeOfRequest(req), req.Host, r.addr)
	targetUrl := fmt.Sprintf("http://%s", profile.KibanaAddress)
	if profile.KibanaMtlsEnabled {
		targetUrl = fmt.Sprintf("https://%s", profile.KibanaAddress)
	}

	proxy := NewKibanaProxy(sourceUrl, targetUrl, profile.KibanaMtlsEnabled)
	proxy.ReverseProxy().ServeHTTP(w, req)
}

func RateLimiter(limiter *rate.Limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !limiter.Allow() {
				http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func NormalizePath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = strings.ReplaceAll(r.URL.Path, "//", "/")
		log.Printf("Normalized path: %s", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

func (r *kibanaRouter) ServeElasticsearch(w http.ResponseWriter, req *http.Request) {
	startTime := time.Now()
	span := opentracing.StartSpan("barito_router_viewer.elasticsearch")
	defer span.Finish()

	log.Println("ServeElasticsearch: Received request")

	vars := mux.Vars(req)
	clusterName := vars["cluster_name"]
	esEndpoint := vars["es_endpoint"]

	log.Printf("ServeElasticsearch: clusterName=%s, esEndpoint=%s", clusterName, esEndpoint)

	if strings.Contains(esEndpoint, "//") {
		esEndpoint = strings.ReplaceAll(esEndpoint, "//", "/")
		log.Printf("Normalized endpoint: %s", esEndpoint)
	}

	decodedEndpoint, err := url.PathUnescape(esEndpoint)
	if err != nil {
		http.Error(w, "Invalid URL", http.StatusBadRequest)
		return
	}
	normalizedEndpoint := path.Clean(decodedEndpoint)
	log.Printf("ServeElasticsearch: normalizedEndpoint: %s", normalizedEndpoint)

	if clusterName == "" {
		http.Error(w, "clusterName is required", http.StatusBadRequest)
		return
	}
	span.SetTag("app-group", clusterName)

	appSecret := req.Header.Get("App-Secret")
	if appSecret == "" {
		http.Error(w, "App-Secret header is required", http.StatusUnauthorized)
		return
	}

	profile, err := fetchProfileByClusterName(r.client, span.Context(), r.cacheBag, r.marketUrl, r.accessToken, r.profilePath, clusterName)
	if err != nil || profile == nil || profile.AppGroupSecret != appSecret {
		http.Error(w, "Invalid app secret or cluster name", http.StatusUnauthorized)
		return
	}

	if profile.ElasticsearchStatus != "ACTIVE" {
		http.Error(w, "Elasticsearch cluster is not active", http.StatusServiceUnavailable)
		return
	}

	if req.Method != http.MethodGet && req.Method != http.MethodPost {
		http.Error(w, "Only GET and POST requests are allowed", http.StatusMethodNotAllowed)
		return
	}

	allowedEndpoints := []string{
		"_search",
		"_search/scroll",
		"_doc",
		"_cat/indices",
		"_eql/search",
		"_mget",
		"_index",
		"_ingest/pipeline",
	}

	isAllowed := false
	for _, allowed := range allowedEndpoints {
		if normalizedEndpoint == allowed || strings.HasPrefix(normalizedEndpoint, allowed+"/") {
			isAllowed = true
			break
		}
	}

	if !isAllowed {
		http.Error(w, "This endpoint is not allowed", http.StatusForbidden)
		return
	}

	if r.limiter == nil {
		log.Println("Limiter is nil")
		http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
		return
	}

	if !r.limiter.Allow() {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}

	esPort := 9200
	targetUrl := fmt.Sprintf("http://%s:%d/%s", profile.ElasticsearchAddress, esPort, normalizedEndpoint)
	log.Printf("Extracted cluster_name: %s, es_endpoint: %s, es_address: %s, normalized_endpoint: %s, target_url: %s", clusterName, esEndpoint, profile.ElasticsearchAddress, normalizedEndpoint, targetUrl)

	esReq, err := http.NewRequest(req.Method, targetUrl, req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	for name, values := range req.Header {
		for _, value := range values {
			esReq.Header.Add(name, value)
		}
	}

	esReq.Header.Add("App-Secret", appSecret)

	esRes, err := r.client.Do(esReq)
	if err != nil {
		http.Error(w, "Elasticsearch is unreachable", http.StatusInternalServerError)
		return
	}
	defer esRes.Body.Close()

	body, err := ioutil.ReadAll(esRes.Body)
	if err != nil {
		onTradeError(w, err)
		return
	}

	w.WriteHeader(esRes.StatusCode)
	w.Write(body)

	duration := time.Since(startTime)
	LogAudit(req, esRes, body, appSecret, clusterName, duration)
}

func (r *kibanaRouter) MustBeAuthorizedMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// check if authorized

		// get required parameters
		params := mux.Vars(req)
		username := strings.Split(req.Context().Value("email").(string), "@")[0]
		clusterName := params["cluster_name"]

		// check to the barito market
		address := fmt.Sprintf("%s/%s", r.marketUrl, r.authorizePath)
		q := url.Values{}
		q.Add("username", username)
		q.Add("cluster_name", clusterName)

		checkReq, _ := http.NewRequest("GET", address, nil)
		checkReq.URL.RawQuery = q.Encode()
		res, err := r.client.Do(checkReq)
		if err != nil {
			onTradeError(w, err)
			return
		}
		if res.StatusCode != http.StatusOK {
			onAuthorizeError(w)
			return
		}

		next.ServeHTTP(w, req)
	})
}

func KibanaGetClustername(req *http.Request) string {
	urlPath := strings.Split(req.URL.Path, "/")
	if len(urlPath) > 1 {
		return urlPath[1]
	}

	return ""
}

func getTargetScheme(srv *api.CatalogService) (scheme string) {
	scheme = srv.NodeMeta["kibana_scheme"]

	if scheme == "" {
		scheme = "http"
	}

	return scheme
}

func (r *kibanaRouter) isUseSSO() bool {
	return r.ssoEnabled
}
