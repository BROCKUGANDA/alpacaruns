// Typed client for the Alpacaruns dashboard API. The shapes here
// MUST stay in lockstep with api/types.go on the Go side — keep them
// side-by-side when adding new fields.

export type BotState = "running" | "halted" | "paused" | "error";

export type HealthResponse = {
  ok: boolean;
  version: string;
  uptime_sec: number;
  last_poll?: string;
  bot_alive: boolean;
};

export type KillSwitch = {
  daily: boolean;
  weekly: boolean;
  total: boolean;
  halted: boolean;
};

export type StatusConfig = {
  max_position_usd: number;
  max_portfolio_pct: number;
  crypto_max_position_usd: number;
  daily_dd_halt: number;
  weekly_dd_halt: number;
  total_dd_halt: number;
  llm_provider: string;
  options_enabled: boolean;
  symbols: string[];
  ensemble_enabled: boolean;
};

export type StatusResponse = {
  bot: BotState;
  kill_switch: KillSwitch;
  config: StatusConfig;
  tick_number: number;
  last_tick?: string;
  last_error?: string;
  paused: boolean;
};

export type AccountResponse = {
  equity: string;
  cash: string;
  day_pnl: string;
  buying_power: string;
  multiplier: string;
  status: string;
  account_number: string;
  created_at?: string;
  last_equity?: string;
  portfolio_value?: string;
};

export type EquitySnapshot = {
  t: string;
  equity: number;
  day_pnl: number;
  drawdown_daily: number;
  drawdown_weekly: number;
  drawdown_total: number;
};

export type PnLSummary = {
  starting_equity: number;
  current_equity: number;
  total_pnl: number;
  max_drawdown: number;
  sharpe: number;
  win_rate: number;
  trades: number;
};

export type PnLResponse = {
  snapshots: EquitySnapshot[];
  summary: PnLSummary;
};

export type TradeRow = {
  id: string;
  ts: string;
  symbol: string;
  side: string;
  qty: string;
  price: string;
  status: string;
  path: "agent" | "ensemble" | "manual" | string;
  confidence?: number;
  factor_scores?: Record<string, number>;
  notional?: number;
  unrealized_pl?: string;
};

export type TradesResponse = {
  trades: TradeRow[];
  next_cursor: number | null;
};

export type DecisionRow = {
  ts: string;
  symbol: string;
  risk: string;
  source: string;
  confidence?: number;
  factor_scores?: Record<string, number>;
  detail?: string;
};

export type DecisionsResponse = {
  decisions: DecisionRow[];
  next_cursor: number | null;
  generated_at: string;
};

export type PositionRow = {
  symbol: string;
  qty: string;
  avg_entry_price: string;
  current_price: string;
  market_value: string;
  unrealized_pl: string;
  unrealized_pl_pct: string;
  change_today?: string;
  side: string;
};

export type PositionsResponse = {
  positions: PositionRow[];
  count: number;
};

export type ControlResponse = {
  action: string;
  paused: boolean;
  tick: number;
  result?: string;
  decision?: DecisionRow;
};

export type ErrorResponse = {
  error: string;
  code?: string;
};

const BASE = process.env.NEXT_PUBLIC_API_URL || "http://localhost:8080";

// Dashboard operator token for the state-changing control endpoints
// (POST /api/control/*). Stored in the browser's localStorage — never
// in the bundle — so a separately-deployed dashboard can authenticate
// without shipping the secret in client JS. Same-origin deployments
// against the embedded Go UI don't need it (the server allows
// same-origin browser fetches outright); set it only when the API
// requires a bearer token (DASHBOARD_TOKEN on the server).
const TOKEN_KEY = "alpacaruns_control_token";

export function getDashboardToken(): string | null {
  try {
    const v = window.localStorage.getItem(TOKEN_KEY);
    return v && v.trim() !== "" ? v : null;
  } catch {
    return null;
  }
}

export function setDashboardToken(token: string): void {
  try {
    if (token.trim() === "") {
      window.localStorage.removeItem(TOKEN_KEY);
    } else {
      window.localStorage.setItem(TOKEN_KEY, token.trim());
    }
  } catch {
    // Private mode / blocked storage: controls still work same-origin.
  }
}

// apiFetch — wrapper around fetch that:
//   - prefixes the API base URL
//   - attaches the operator bearer token on control POSTs when set
//   - sends Content-Type when there's a body
//   - throws on non-2xx with a typed Error carrying the API code
// Used by every SWR fetcher so error handling is uniform.
export async function apiFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers({ "Content-Type": "application/json" });
  if (init?.headers) {
    new Headers(init.headers).forEach((v, k) => {
      if (!headers.has(k)) headers.set(k, v);
    });
  }
  // Only control endpoints need auth; read endpoints stay anonymous
  // so the dashboard renders without any setup.
  if (path.startsWith("/api/control/") && (init?.method || "GET") !== "GET") {
    const token = typeof window !== "undefined" ? getDashboardToken() : null;
    if (token && !headers.has("Authorization")) {
      headers.set("Authorization", `Bearer ${token}`);
    }
  }
  const res = await fetch(BASE + path, {
    ...init,
    headers,
    cache: "no-store",
  });
  if (!res.ok) {
    let msg = `HTTP ${res.status}`;
    try {
      const err = (await res.json()) as ErrorResponse;
      msg = err.error || msg;
    } catch {
      // ignore body parse errors
    }
    throw new ApiError(res.status, msg);
  }
  return res.json() as Promise<T>;
}

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
    this.name = "ApiError";
  }
}

// Convenience fetcher for the most common endpoints. SWR's
// `fetcher` prop accepts any (url) => Promise<T> function.
export const swrFetcher = <T>(path: string) => apiFetch<T>(path);