package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/adaptor"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/joho/godotenv"
	cache "github.com/patrickmn/go-cache"
	x402 "github.com/x402-foundation/x402/go/v2"
	x402http "github.com/x402-foundation/x402/go/v2/http"
	x402nethttp "github.com/x402-foundation/x402/go/v2/http/nethttp"
	evmserver "github.com/x402-foundation/x402/go/v2/mechanisms/evm/exact/server"
	"golang.org/x/sync/singleflight"
)

const (
	rpcTimeout       = 1500 * time.Millisecond
	cacheTTL         = 2 * time.Second
	cacheCleanup     = 10 * time.Second
	healthMethod     = "getUserAccountData"
	usdBaseDecimals  = 8
	healthFactorWAD  = 18
	percentageDigits = 2
)

const poolABIJSON = `[{
  "inputs":[{"internalType":"address","name":"user","type":"address"}],
  "name":"getUserAccountData",
  "outputs":[
    {"internalType":"uint256","name":"totalCollateralBase","type":"uint256"},
    {"internalType":"uint256","name":"totalDebtBase","type":"uint256"},
    {"internalType":"uint256","name":"availableBorrowsBase","type":"uint256"},
    {"internalType":"uint256","name":"currentLiquidationThreshold","type":"uint256"},
    {"internalType":"uint256","name":"ltv","type":"uint256"},
    {"internalType":"uint256","name":"healthFactor","type":"uint256"}
  ],
  "stateMutability":"view",
  "type":"function"
}]`

var maxUint256 = new(big.Int).Sub(
	new(big.Int).Lsh(big.NewInt(1), 256),
	big.NewInt(1),
)

type chainConfig struct {
	Key            string
	Name           string
	ChainID        int64
	Network        x402.Network
	Pool           common.Address
	RPCEnv         string
	DefaultRPCURLs []string
}

var supportedChains = []chainConfig{
	{
		Key:     "arbitrum",
		Name:    "Arbitrum One",
		ChainID: 42161,
		Network: x402.Network("eip155:42161"),
		Pool:    common.HexToAddress("0x794a61358D6845594F94dc1DB02A252b5b4814aD"),
		RPCEnv:  "ARBITRUM_RPC_URLS",
		DefaultRPCURLs: []string{
			"https://arb1.arbitrum.io/rpc",
			"https://arbitrum-one-rpc.publicnode.com",
		},
	},
	{
		Key:     "base",
		Name:    "Base",
		ChainID: 8453,
		Network: x402.Network("eip155:8453"),
		Pool:    common.HexToAddress("0xA238Dd80C259a72e81d7e4664a9801593F98d1c5"),
		RPCEnv:  "BASE_RPC_URLS",
		DefaultRPCURLs: []string{
			"https://mainnet.base.org",
			"https://base-rpc.publicnode.com",
		},
	},
	{
		Key:     "polygon",
		Name:    "Polygon",
		ChainID: 137,
		Network: x402.Network("eip155:137"),
		Pool:    common.HexToAddress("0x794a61358D6845594F94dc1DB02A252b5b4814aD"),
		RPCEnv:  "POLYGON_RPC_URLS",
		DefaultRPCURLs: []string{
			"https://polygon-bor-rpc.publicnode.com",
			"https://polygon-rpc.com",
		},
	},
}

type rpcEndpoint struct {
	url    string
	mu     sync.Mutex
	client *ethclient.Client
}

func (endpoint *rpcEndpoint) getClient(ctx context.Context) (*ethclient.Client, error) {
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	if endpoint.client != nil {
		return endpoint.client, nil
	}

	client, err := ethclient.DialContext(ctx, endpoint.url)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", endpoint.url, err)
	}
	endpoint.client = client
	return client, nil
}

func (endpoint *rpcEndpoint) close() {
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	if endpoint.client != nil {
		endpoint.client.Close()
		endpoint.client = nil
	}
}

type chainProvider struct {
	config    chainConfig
	endpoints []*rpcEndpoint
}

type accountData struct {
	totalCollateralBase         *big.Int
	totalDebtBase               *big.Int
	availableBorrowsBase        *big.Int
	currentLiquidationThreshold *big.Int
	ltv                         *big.Int
	healthFactor                *big.Int
}

type healthResponse struct {
	Chain                       string    `json:"chain"`
	ChainID                     int64     `json:"chain_id"`
	Wallet                      string    `json:"wallet"`
	HealthFactor                float64   `json:"health_factor"`
	TotalCollateralUSD          float64   `json:"total_collateral_usd"`
	TotalDebtUSD                float64   `json:"total_debt_usd"`
	AvailableBorrowsUSD         float64   `json:"available_borrows_usd"`
	CurrentLiquidationThreshold float64   `json:"current_liquidation_threshold_pct"`
	LTV                         float64   `json:"ltv_pct"`
	UpdatedAt                   time.Time `json:"updated_at"`
	Cached                      bool      `json:"cached"`
}

type apiService struct {
	poolABI   abi.ABI
	providers map[string]*chainProvider
	cache     *cache.Cache
	inflight  singleflight.Group
}

func newAPIService(rpcOverrides map[string][]string) (*apiService, error) {
	parsedABI, err := abi.JSON(strings.NewReader(poolABIJSON))
	if err != nil {
		return nil, fmt.Errorf("parse Aave Pool ABI: %w", err)
	}

	service := &apiService{
		poolABI:   parsedABI,
		providers: make(map[string]*chainProvider, len(supportedChains)),
		cache:     cache.New(cacheTTL, cacheCleanup),
	}
	for _, config := range supportedChains {
		urls := append([]string(nil), rpcOverrides[config.Key]...)
		if envURLs := parseCommaSeparated(os.Getenv(config.RPCEnv)); len(envURLs) > 0 {
			urls = append(envURLs, urls...)
		}
		urls = append(urls, config.DefaultRPCURLs...)
		urls = uniqueStrings(urls)
		if len(urls) == 0 {
			return nil, fmt.Errorf("no RPC URLs configured for %s", config.Name)
		}

		provider := &chainProvider{config: config}
		for _, url := range urls {
			provider.endpoints = append(provider.endpoints, &rpcEndpoint{url: url})
		}
		service.providers[config.Key] = provider
	}
	return service, nil
}

func loadRPCOverrides() (map[string][]string, error) {
	overrides := make(map[string][]string)
	raw := strings.TrimSpace(os.Getenv("RPC_URLS"))
	if raw == "" {
		return overrides, nil
	}
	if err := json.Unmarshal([]byte(raw), &overrides); err != nil {
		return nil, fmt.Errorf("RPC_URLS must be a JSON object of chain keys to URL arrays: %w", err)
	}
	for chain, urls := range overrides {
		if !isSupportedChain(chain) {
			return nil, fmt.Errorf("RPC_URLS contains unsupported chain %q", chain)
		}
		if len(uniqueStrings(urls)) == 0 {
			return nil, fmt.Errorf("RPC_URLS for %q has no URLs", chain)
		}
	}
	return overrides, nil
}

func parseCommaSeparated(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	urls := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			urls = append(urls, trimmed)
		}
	}
	return urls
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func isSupportedChain(chain string) bool {
	for _, config := range supportedChains {
		if config.Key == chain {
			return true
		}
	}
	return false
}

func (service *apiService) close() {
	for _, provider := range service.providers {
		for _, endpoint := range provider.endpoints {
			endpoint.close()
		}
	}
}

func (service *apiService) fetchAccountData(
	ctx context.Context,
	provider *chainProvider,
	wallet common.Address,
) (*accountData, error) {
	callData, err := service.poolABI.Pack(healthMethod, wallet)
	if err != nil {
		return nil, fmt.Errorf("encode Aave Pool call: %w", err)
	}

	type rpcResult struct {
		data *accountData
		err  error
		url  string
	}
	results := make(chan rpcResult, len(provider.endpoints))
	var wait sync.WaitGroup
	for _, endpoint := range provider.endpoints {
		wait.Add(1)
		go func(endpoint *rpcEndpoint) {
			defer wait.Done()
			callCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
			defer cancel()

			client, clientErr := endpoint.getClient(callCtx)
			if clientErr != nil {
				results <- rpcResult{err: clientErr, url: endpoint.url}
				return
			}
			output, callErr := client.CallContract(
				callCtx,
				ethereum.CallMsg{To: &provider.config.Pool, Data: callData},
				nil,
			)
			if callErr != nil {
				results <- rpcResult{
					err: fmt.Errorf("RPC %s Aave call: %w", endpoint.url, callErr),
					url: endpoint.url,
				}
				return
			}
			data, unpackErr := unpackAccountData(service.poolABI, output)
			results <- rpcResult{data: data, err: unpackErr, url: endpoint.url}
		}(endpoint)
	}
	go func() {
		wait.Wait()
		close(results)
	}()

	var failures []string
	for result := range results {
		if result.err == nil {
			return result.data, nil
		}
		failures = append(failures, result.err.Error())
	}
	if len(failures) == 0 {
		return nil, errors.New("no RPC endpoints are configured")
	}
	return nil, fmt.Errorf("all RPC endpoints failed: %s", strings.Join(failures, "; "))
}

func unpackAccountData(parsedABI abi.ABI, output []byte) (*accountData, error) {
	values, err := parsedABI.Unpack(healthMethod, output)
	if err != nil {
		return nil, fmt.Errorf("decode Aave Pool response: %w", err)
	}
	if len(values) != 6 {
		return nil, fmt.Errorf("Aave Pool returned %d fields; expected 6", len(values))
	}

	fields := make([]*big.Int, len(values))
	for index, value := range values {
		field, ok := value.(*big.Int)
		if !ok || field == nil {
			return nil, fmt.Errorf("Aave Pool field %d has unexpected type %T", index, value)
		}
		fields[index] = field
	}
	return &accountData{
		totalCollateralBase:         fields[0],
		totalDebtBase:               fields[1],
		availableBorrowsBase:        fields[2],
		currentLiquidationThreshold: fields[3],
		ltv:                         fields[4],
		healthFactor:                fields[5],
	}, nil
}

func scaledFloat(value *big.Int, decimals int, precision int) (float64, error) {
	if value == nil || value.Sign() < 0 {
		return 0, errors.New("value must be a non-negative integer")
	}
	divisor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	rational := new(big.Rat).SetFrac(value, divisor)
	result, err := strconv.ParseFloat(rational.FloatString(precision), 64)
	if err != nil || math.IsInf(result, 0) || math.IsNaN(result) {
		return 0, fmt.Errorf("value does not fit in a JSON number")
	}
	return result, nil
}

func buildHealthResponse(
	config chainConfig,
	wallet common.Address,
	data *accountData,
) (*healthResponse, error) {
	if data.healthFactor.Sign() == 0 || data.healthFactor.Cmp(maxUint256) == 0 {
		return nil, errors.New("No active Aave position found")
	}

	healthFactor, err := scaledFloat(data.healthFactor, healthFactorWAD, 4)
	if err != nil {
		return nil, fmt.Errorf("convert health factor: %w", err)
	}
	collateral, err := scaledFloat(data.totalCollateralBase, usdBaseDecimals, 2)
	if err != nil {
		return nil, fmt.Errorf("convert total collateral: %w", err)
	}
	debt, err := scaledFloat(data.totalDebtBase, usdBaseDecimals, 2)
	if err != nil {
		return nil, fmt.Errorf("convert total debt: %w", err)
	}
	borrows, err := scaledFloat(data.availableBorrowsBase, usdBaseDecimals, 2)
	if err != nil {
		return nil, fmt.Errorf("convert available borrows: %w", err)
	}
	liquidationThreshold, err := scaledFloat(data.currentLiquidationThreshold, percentageDigits, 2)
	if err != nil {
		return nil, fmt.Errorf("convert liquidation threshold: %w", err)
	}
	ltv, err := scaledFloat(data.ltv, percentageDigits, 2)
	if err != nil {
		return nil, fmt.Errorf("convert LTV: %w", err)
	}

	return &healthResponse{
		Chain:                       config.Key,
		ChainID:                     config.ChainID,
		Wallet:                      wallet.Hex(),
		HealthFactor:                healthFactor,
		TotalCollateralUSD:          collateral,
		TotalDebtUSD:                debt,
		AvailableBorrowsUSD:         borrows,
		CurrentLiquidationThreshold: liquidationThreshold,
		LTV:                         ltv,
		UpdatedAt:                   time.Now().UTC(),
	}, nil
}

func (service *apiService) getHealth(
	ctx context.Context,
	chain string,
	wallet common.Address,
) (*healthResponse, bool, error) {
	key := chain + ":" + strings.ToLower(wallet.Hex())
	if value, ok := service.cache.Get(key); ok {
		response, valid := value.(healthResponse)
		if valid {
			response.Cached = true
			return &response, true, nil
		}
		log.Printf("discarding unexpected cache value for key %s", key)
		service.cache.Delete(key)
	}

	result := service.inflight.DoChan(key, func() (interface{}, error) {
		if value, ok := service.cache.Get(key); ok {
			if response, valid := value.(healthResponse); valid {
				return response, nil
			}
		}
		provider := service.providers[chain]
		data, err := service.fetchAccountData(ctx, provider, wallet)
		if err != nil {
			return nil, err
		}
		response, err := buildHealthResponse(provider.config, wallet, data)
		if err != nil {
			return nil, err
		}
		service.cache.Set(key, *response, cacheTTL)
		return *response, nil
	})

	select {
	case <-ctx.Done():
		return nil, false, ctx.Err()
	case result := <-result:
		if result.Err != nil {
			return nil, false, result.Err
		}
		response, ok := result.Val.(healthResponse)
		if !ok {
			return nil, false, fmt.Errorf("unexpected health response type %T", result.Val)
		}
		return &response, response.Cached, nil
	}
}

func validateWallet(value string) (common.Address, error) {
	if len(value) != 42 || !common.IsHexAddress(value) {
		return common.Address{}, errors.New("wallet must be a 42-character 0x-prefixed EVM address")
	}
	return common.HexToAddress(value), nil
}

func (service *apiService) healthHandler(w http.ResponseWriter, request *http.Request) {
	wallet, err := validateWallet(request.URL.Query().Get("wallet"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	chain := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("chain")))
	_, ok := service.providers[chain]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "chain must be one of: arbitrum, base, polygon",
		})
		return
	}

	response, cached, err := service.getHealth(request.Context(), chain, wallet)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			writeJSON(w, http.StatusGatewayTimeout, map[string]string{
				"error": "Aave RPC request timed out",
			})
			return
		}
		if err.Error() == "No active Aave position found" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		log.Printf("Aave query failed for chain=%s wallet=%s: %v", chain, wallet.Hex(), err)
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "Aave RPC request failed",
		})
		return
	}
	response.Cached = cached
	writeJSON(w, http.StatusOK, response)
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write JSON response failed: %v", err)
	}
}

func buildPaymentMiddleware(
	wallet,
	facilitatorURL string,
) fiber.Handler {
	accepted := make(x402http.PaymentOptions, 0, len(supportedChains))
	chainNames := make([]string, 0, len(supportedChains))
	for _, config := range supportedChains {
		accepted = append(accepted, x402http.PaymentOption{
			Scheme:            "exact",
			PayTo:             wallet,
			Price:             "$0.01",
			Network:           config.Network,
			MaxTimeoutSeconds: 30,
		})
		chainNames = append(chainNames, config.Key)
	}

	routes := x402http.RoutesConfig{
		"* /*": {
			Accepts:     accepted,
			Description: "Aave V3 Health API",
			MimeType:    "application/json",
			UnpaidResponseBody: func(
				_ context.Context,
				_ x402http.HTTPRequestContext,
			) (*x402http.UnpaidResponse, error) {
				return &x402http.UnpaidResponse{
					ContentType: "application/json",
					Body: map[string]interface{}{
						"error":            "Payment Required",
						"protocol":         "x402",
						"price":            "0.01 USDC",
						"wallet":           wallet,
						"supported_chains": chainNames,
					},
				}, nil
			},
		},
	}

	facilitator := x402http.NewHTTPFacilitatorClient(&x402http.FacilitatorConfig{
		URL:     facilitatorURL,
		Timeout: 10 * time.Second,
	})
	options := []x402nethttp.MiddlewareOption{
		x402nethttp.WithFacilitatorClient(facilitator),
		x402nethttp.WithSyncFacilitatorOnStart(false),
		x402nethttp.WithTimeout(10 * time.Second),
	}
	for _, config := range supportedChains {
		options = append(
			options,
			x402nethttp.WithScheme(config.Network, evmserver.NewExactEvmScheme()),
		)
	}
	middleware := x402nethttp.PaymentMiddlewareFromConfig(routes, options...)
	paymentHandler := adaptor.HTTPMiddleware(middleware)
	return func(c *fiber.Ctx) error {
		if c.Path() == "/ping" {
			return c.Next()
		}
		return paymentHandler(c)
	}
}

func newFiberApp(service *apiService, wallet, facilitatorURL string) *fiber.App {
	app := fiber.New(fiber.Config{
		AppName:      "Aave V3 Health Factor API",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	})
	app.Use(func(c *fiber.Ctx) error {
		started := time.Now()
		err := c.Next()
		c.Set("X-Response-Time-Ms", strconv.FormatFloat(
			float64(time.Since(started).Microseconds())/1000,
			'f',
			3,
			64,
		))
		return err
	})
	app.Use(recover.New())
	app.Use(buildPaymentMiddleware(wallet, facilitatorURL))

	app.Get("/health", func(c *fiber.Ctx) error {
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"status": "ok"})
	})
	app.Get("/ping", func(c *fiber.Ctx) error {
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"status": "ok"})
	})
	app.Get("/", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"name":    "Aave V3 Health Factor API",
			"message": "Use /api/v1/aave/health?wallet=0x...&chain=base",
		})
	})
	app.Get("/api/v1/aave/health", adaptor.HTTPHandler(http.HandlerFunc(service.healthHandler)))
	return app
}

func main() {
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("could not load .env: %v", err)
	}
	wallet := strings.TrimSpace(os.Getenv("X402_WALLET_ADDRESS"))
	if _, err := validateWallet(wallet); err != nil {
		log.Fatalf("invalid X402_WALLET_ADDRESS: %v", err)
	}
	rpcOverrides, err := loadRPCOverrides()
	if err != nil {
		log.Fatalf("invalid RPC configuration: %v", err)
	}
	service, err := newAPIService(rpcOverrides)
	if err != nil {
		log.Fatalf("initialize API service: %v", err)
	}
	defer service.close()

	facilitatorURL := strings.TrimSpace(os.Getenv("X402_FACILITATOR_URL"))
	if facilitatorURL == "" {
		facilitatorURL = x402http.DefaultFacilitatorURL
	}
	app := newFiberApp(service, wallet, facilitatorURL)
	address := strings.TrimSpace(os.Getenv("HTTP_ADDR"))
	if address == "" {
		address = ":8080"
	}

	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- app.Listen(address)
	}()
	log.Printf(
		"Aave Health API listening on %s; x402 receiver=%s facilitator=%s",
		address,
		wallet,
		facilitatorURL,
	)

	shutdown, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()
	select {
	case err := <-serverErrors:
		if err != nil {
			log.Printf("HTTP server stopped: %v", err)
		}
	case <-shutdown.Done():
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := app.ShutdownWithContext(ctx); err != nil {
			log.Printf("graceful shutdown failed: %v", err)
		}
	}
}
