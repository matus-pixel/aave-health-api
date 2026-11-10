# Aave V3 Health Factor & Collateral API

A read-only, paid API for Aave V3 account data on Arbitrum One, Base, and
Polygon. It calls each network's Aave V3 Pool `getUserAccountData(address)`
view method through go-ethereum. It never signs or sends transactions.

The `GET /api/v1/aave/health` route is protected by x402 and costs **0.01
USDC per successful request**. x402 payment signatures are validated and
settled by the configured facilitator; merely sending a payment header does
not grant access. `/health` is free.

## Requirements and configuration

- Go 1.26.10 or later (the x402 Go SDK currently requires Go 1.26.10).
- An EVM-compatible address to receive payments. The same address is used on
  all supported networks.
- Public RPC URLs work for development; production traffic should use reliable
  RPC providers and a production x402 facilitator.

```powershell
Set-Location C:\dev\aave-health-api
Copy-Item .env.example .env
# Edit .env and replace X402_WALLET_ADDRESS with your receiving address.
go mod download
go build -o aave-health-api.exe .
.\aave-health-api.exe
```

`RPC_URLS` is a JSON object mapping `arbitrum`, `base`, and `polygon` to ordered
arrays of RPC URLs. Optional `ARBITRUM_RPC_URLS`, `BASE_RPC_URLS`, and
`POLYGON_RPC_URLS` environment variables accept comma-separated URLs and are
tried before `RPC_URLS` entries and built-in public fallbacks. If a node fails,
requests are dispatched concurrently to all configured endpoints and the first
successful Aave response is used.

Set `X402_FACILITATOR_URL` to the facilitator you use in production. The
default `https://x402.org/facilitator` is the public x402 facilitator; confirm
its supported networks and operational terms before production use. Configure
any required credentials using the authentication method required by your
facilitator.

## Endpoints

### Free health check

```powershell
curl.exe -i http://localhost:8080/health
```

Expected body:

```json
{"status":"ok"}
```

### Paid Aave query

A request without payment returns HTTP 402, the standard x402 payment
requirements (including the accepted EVM networks) and a JSON summary:

```powershell
curl.exe -i "http://localhost:8080/api/v1/aave/health?wallet=0x0000000000000000000000000000000000000001&chain=arbitrum"
```

The JSON summary includes the receiving wallet, `0.01 USDC` price, and
supported chain keys. An x402-compatible buyer signs a payment for one of the
advertised exact-payment options and retries the same request with the
protocol payment header. The x402 resource-server middleware asks the
facilitator to verify and settle it; only then does the API handler query
Aave and return account data.

For example, after a buyer obtains a signed x402 payment payload from its
wallet/client SDK:

```powershell
curl.exe -i `
  -H "PAYMENT-SIGNATURE: <signed-x402-payload>" `
  "http://localhost:8080/api/v1/aave/health?wallet=0x0000000000000000000000000000000000000001&chain=base"
```

The placeholder above is explanatory only; request wallets should use the
official x402 buyer SDK to create and submit protocol-compliant signed
payments. The returned `X-Response-Time-Ms` header reports total API response
time.

## Response fields and units

- `health_factor`: Aave's 18-decimal WAD value, rounded to four decimal places.
- `total_collateral_usd`, `total_debt_usd`, `available_borrows_usd`: Aave base
  currency values (8 decimals), rounded to two decimal places.
- `current_liquidation_threshold_pct`, `ltv_pct`: Aave's basis-point values
  converted to percentages and rounded to two decimals.
- `cached`: whether the response came from the two-second in-memory cache.

No position returns HTTP 404 with
`{"error":"No active Aave position found"}`. Invalid input returns HTTP 400;
an RPC outage or deadline returns HTTP 502 or 504. Every response includes
`X-Response-Time-Ms`.

## Liquidator / risk-bot integration

An x402-capable bot should first request the endpoint. On HTTP 402, it reads
the advertised requirements, selects the matching chain and exact USDC
payment option, and uses an x402 buyer SDK and funded wallet to create the
payment payload. The bot retries the identical URL with that payload, then
parses `health_factor`, collateral, debt, and available borrowing amounts.
The API caches results for at most two seconds, so risk-sensitive clients
should factor that freshness window into their own execution policy.

This endpoint supplies account-level risk data only. It does not identify
liquidatable collateral positions, estimate liquidation profitability, or
execute liquidations.

## Docker

```powershell
docker build -t aave-health-api .
docker run --rm -p 8080:8080 --env-file .env aave-health-api
```
