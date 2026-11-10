package main

import (
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestScaledFloatUsesAaveUnitsAndRounding(t *testing.T) {
	value, err := scaledFloat(big.NewInt(12345678901), usdBaseDecimals, 2)
	if err != nil {
		t.Fatalf("scaledFloat returned error: %v", err)
	}
	if value != 123.46 {
		t.Fatalf("USD conversion = %v, want 123.46", value)
	}

	health, err := scaledFloat(big.NewInt(1234567890123456789), healthFactorWAD, 4)
	if err != nil {
		t.Fatalf("health factor conversion returned error: %v", err)
	}
	if health != 1.2346 {
		t.Fatalf("health factor = %v, want 1.2346", health)
	}
}

func TestBuildHealthResponseAndNoPositionSentinels(t *testing.T) {
	config := supportedChains[0]
	wallet := common.HexToAddress("0x0000000000000000000000000000000000000001")
	data := &accountData{
		totalCollateralBase:         big.NewInt(123456789),
		totalDebtBase:               big.NewInt(50000000),
		availableBorrowsBase:        big.NewInt(25000000),
		currentLiquidationThreshold: big.NewInt(8250),
		ltv:                         big.NewInt(7500),
		healthFactor:                new(big.Int).Mul(big.NewInt(15), new(big.Int).Exp(big.NewInt(10), big.NewInt(17), nil)),
	}
	response, err := buildHealthResponse(config, wallet, data)
	if err != nil {
		t.Fatalf("buildHealthResponse returned error: %v", err)
	}
	if response.TotalCollateralUSD != 1.23 || response.TotalDebtUSD != 0.50 {
		t.Fatalf("unexpected USD conversions: collateral=%v debt=%v",
			response.TotalCollateralUSD, response.TotalDebtUSD)
	}
	if response.HealthFactor != 1.5 || response.CurrentLiquidationThreshold != 82.5 || response.LTV != 75 {
		t.Fatalf("unexpected risk factors: %+v", response)
	}

	for _, value := range []*big.Int{
		big.NewInt(0),
		new(big.Int).Set(maxUint256),
	} {
		data.healthFactor = value
		if _, err := buildHealthResponse(config, wallet, data); err == nil ||
			err.Error() != "No active Aave position found" {
			t.Fatalf("health factor %s should return no-position error, got %v", value, err)
		}
	}
}

func TestValidateWallet(t *testing.T) {
	if _, err := validateWallet("0x0000000000000000000000000000000000000001"); err != nil {
		t.Fatalf("valid wallet rejected: %v", err)
	}
	for _, wallet := range []string{
		"",
		"0x1234",
		"0000000000000000000000000000000000000001",
		"0xzz00000000000000000000000000000000000001",
	} {
		if _, err := validateWallet(wallet); err == nil {
			t.Errorf("invalid wallet %q was accepted", wallet)
		}
	}
}

func TestFreeHealthAndPaidChallenge(t *testing.T) {
	service, err := newAPIService(nil)
	if err != nil {
		t.Fatalf("newAPIService returned error: %v", err)
	}
	defer service.close()

	app := newFiberApp(
		service,
		"0x0000000000000000000000000000000000000001",
		"http://127.0.0.1:1",
	)
	healthRequest := httptest.NewRequest(http.MethodGet, "/health", nil)
	healthResponse, err := app.Test(healthRequest)
	if err != nil {
		t.Fatalf("health request failed: %v", err)
	}
	defer healthResponse.Body.Close()
	if healthResponse.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d, want 200", healthResponse.StatusCode)
	}
	if healthResponse.Header.Get("X-Response-Time-Ms") == "" {
		t.Fatal("health response is missing X-Response-Time-Ms")
	}

	paidRequest := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/aave/health?wallet=0x0000000000000000000000000000000000000001&chain=arbitrum",
		nil,
	)
	paidResponse, err := app.Test(paidRequest)
	if err != nil {
		t.Fatalf("unpaid API request failed: %v", err)
	}
	defer paidResponse.Body.Close()
	if paidResponse.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("unpaid API status = %d, want 402", paidResponse.StatusCode)
	}
	if paidResponse.Header.Get("X-Response-Time-Ms") == "" {
		t.Fatal("402 response is missing X-Response-Time-Ms")
	}
	if paidResponse.Header.Get("PAYMENT-REQUIRED") == "" {
		t.Fatal("402 response is missing x402 payment requirements")
	}
	var body map[string]interface{}
	if err := json.NewDecoder(paidResponse.Body).Decode(&body); err != nil {
		t.Fatalf("decode 402 body: %v", err)
	}
	if body["wallet"] != "0x0000000000000000000000000000000000000001" ||
		body["price"] != "0.01 USDC" {
		t.Fatalf("402 body missing configured wallet or price: %+v", body)
	}
}

func TestScaledFloatRejectsUnrepresentableValues(t *testing.T) {
	value := new(big.Int).Lsh(big.NewInt(1), 2048)
	_, err := scaledFloat(value, 0, 2)
	if err == nil || !strings.Contains(err.Error(), "does not fit") {
		t.Fatalf("expected unrepresentable value error, got %v", err)
	}
}
