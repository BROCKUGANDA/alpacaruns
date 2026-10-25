#!/usr/bin/env bash
# Live order-type verification against the PAPER account.
set -uo pipefail
cd /opt/alpacaruns
run() { echo "--- $*"; sudo -u alpacaruns /usr/local/bin/alpacaruns trade --env /opt/alpacaruns/.env "$@" 2>&1 | tail -3; }

echo "=== 1. BUY MARKET (crypto, 24/7) ==="
run --symbol BTC/USD --side buy --qty 0.0001 --type market
echo "=== 2. SELL MARKET (close it) ==="
run --symbol BTC/USD --side sell --qty 0.0001 --type market
echo "=== 3. BUY LIMIT (far below market) ==="
run --symbol BTC/USD --side buy --qty 0.0001 --type limit --limit 50000
echo "=== 4. SELL STOP_LIMIT (crypto) ==="
run --symbol BTC/USD --side sell --qty 0.0001 --type stop_limit --stop-price 40000 --limit 39000
echo "=== 5. invalid combinations refused locally ==="
run --symbol BTC/USD --side buy --qty 0.0001 --type market --limit 100
run --symbol BTC/USD --side buy --qty 0.0001 --type stop_limit --stop-price 40000
echo "=== 6. EQUITY limit / stop_limit outside market hours ==="
run --symbol SPY --side buy --qty 1 --type limit --limit 0.01
run --symbol SPY --side sell --qty 1 --type stop_limit --stop-price 999 --limit 998
