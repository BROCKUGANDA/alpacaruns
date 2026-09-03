#!/usr/bin/env python3
"""Generate narration MP3 — final pass targeting ~3:00."""
import asyncio, edge_tts, os
from pathlib import Path

OUT_DIR = r"C:/Users/HP/Desktop/Alpacaruns/hackathon_video"
SLIDE_MP3_DIR = os.path.join(OUT_DIR, "slide_audio")
os.makedirs(SLIDE_MP3_DIR, exist_ok=True)

SLIDES = [
    # 1 — title (33w)
    "Alpacaruns is an autonomous trading system for Alpaca, written in Go. "
    "A Mixture-of-Experts multi-agent graph on Google ADK for Go, "
    "calling Alpaca through the official MCP server. "
    "Paper trading is the default, one journal explains every fill.",

    # 2 — problem (35w)
    "Most retail AI-trading demos hide what they decided and why. "
    "LLM agents fail silently — malformed tool calls, missing prices, drifted state. "
    "Alpaca's free tier blocks options snapshots, breaking naive bots. "
    "Alpacaruns is built the other way: fail-closed, auditable.",

    # 3 — what it does (38w)
    "Two execution paths, one shared risk gate. "
    "The LLM path is a SequentialAgent: market data, technical and sentiment in parallel, "
    "a trade idea, a risk check, and execution through the MCP server. "
    "The auto path is pure: five factors, threshold rule, fixed-fractional sizing.",

    # 4 — architecture (40w)
    "Here is the architecture. GatingRoot routes ticks and questions. "
    "On a tick, MarketData feeds Analysis — technical and news in parallel — "
    "then a TradeIdea expert, Risk Management, and Execution through the MCP server. "
    "Risk Management loops back until the kill switch engages.",

    # 5 — risk (38w)
    "Risk is where we spent the most time. A single Go validator gates every order. "
    "Position caps, portfolio caps, a confidence floor, a multi-factor gate. "
    "Drawdown halts fire at five percent daily, ten weekly, fifteen of peak equity. "
    "Idempotent order IDs prevent duplicate sells.",

    # 6 — tech stack (34w)
    "Stack: Go, Google ADK for Go, Alpaca through the MCP server. "
    "Local LLM is Qwen3 four-billion via llama.cpp with the Jinja template; "
    "we also support Gemini and gpt-oss-twenty-b. "
    "State is a JSONL journal plus versioned strategy state.",

    # 7 — constraints + demo (44w)
    "Honest constraints: every result is paper-only, no live capital traded. "
    "Options degrade to equity on the free tier. No backtester yet. "
    "Here is the live journal — each line a real decision. "
    "An approved buy with TP and SL inline, or a blocked entry with the full vote trail. "
    "alpacaruns auto dry-run replays the pipeline without orders.",
]

VOICE = "en-US-GuyNeural"
RATE  = "-3%"

async def synth_one(idx: int, body: str) -> Path:
    out_path = Path(SLIDE_MP3_DIR) / f"slide_{idx:02d}.mp3"
    text = f"<speak version='1.0' xml:lang='en-US'><voice name='{VOICE}'><prosody rate='{RATE}'>{body}</prosody></voice></speak>"
    comm = edge_tts.Communicate(text, voice=VOICE, rate=RATE)
    await comm.save(str(out_path))
    return out_path

async def main():
    paths = []
    for i, body in enumerate(SLIDES, 1):
        p = await synth_one(i, body)
        paths.append(p)
        wc = len(body.split())
        print(f"  slide {i}: {wc:>3}w  -> {p.name}")
    concat_file = Path(OUT_DIR) / "concat.txt"
    with open(concat_file, "w", encoding="utf-8") as f:
        for p in paths:
            f.write(f"file '{p.resolve().as_posix()}'\n")
    print("wrote concat list:", concat_file)

asyncio.run(main())
