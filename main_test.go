package main

import (
	"encoding/json"
	"io"
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

func TestGlobalPaymentChallenge(t *testing.T) {
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

	pingRequest := httptest.NewRequest(http.MethodGet, "/ping", nil)
	pingResponse, err := app.Test(pingRequest)
	if err != nil {
		t.Fatalf("ping request failed: %v", err)
	}
	pingBody, err := io.ReadAll(pingResponse.Body)
	pingResponse.Body.Close()
	if err != nil {
		t.Fatalf("read ping response: %v", err)
	}
	if pingResponse.StatusCode != http.StatusOK {
		t.Fatalf("ping status = %d, want 200", pingResponse.StatusCode)
	}
	if string(pingBody) != `{"status":"ok"}` {
		t.Fatalf("ping body = %q, want %q", pingBody, `{"status":"ok"}`)
	}
	if pingResponse.Header.Get("PAYMENT-REQUIRED") != "" {
		t.Fatal("ping response unexpectedly includes x402 payment requirements")
	}

	for _, path := range []string{
		"/",
		"/health",
		"/not-found",
		"/api/v1/aave/health?wallet=0x0000000000000000000000000000000000000001&chain=arbitrum",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response, err := app.Test(request)
		if err != nil {
			t.Fatalf("request to %s failed: %v", path, err)
		}
		if response.StatusCode != http.StatusPaymentRequired {
			response.Body.Close()
			t.Errorf("unpaid request to %s returned %d, want 402", path, response.StatusCode)
			continue
		}
		if response.Header.Get("X-Response-Time-Ms") == "" {
			t.Errorf("402 response for %s is missing X-Response-Time-Ms", path)
		}
		if response.Header.Get("PAYMENT-REQUIRED") == "" {
			t.Errorf("402 response for %s is missing x402 payment requirements", path)
		}
		if path == "/" {
			var body map[string]interface{}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Errorf("decode root payment challenge: %v", err)
			} else if body["error"] != "Payment Required" {
				t.Errorf("root request returned handler body instead of payment challenge: %+v", body)
			}
		}
		response.Body.Close()
	}
}

func TestScaledFloatRejectsUnrepresentableValues(t *testing.T) {
	value := new(big.Int).Lsh(big.NewInt(1), 2048)
	_, err := scaledFloat(value, 0, 2)
	if err == nil || !strings.Contains(err.Error(), "does not fit") {
		t.Fatalf("expected unrepresentable value error, got %v", err)
	}
}
