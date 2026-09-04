#!/usr/bin/env python
"""Generate narration audio for the v2 demo slides using edge-tts.

Source: hard-coded narration dict (per-slide).
Output: hackathon_video/slide_audio_v2/slide_NN.mp3

Uses edge-tts (Microsoft Edge's TTS, free, no API key). Each script
section is rendered to an mp3 with a 250ms padding tail.
"""
from __future__ import annotations

import asyncio
import os
from pathlib import Path

import edge_tts

ROOT = Path(__file__).resolve().parent.parent
OUT = ROOT / "hackathon_video" / "slide_audio_v2"
OUT.mkdir(parents=True, exist_ok=True)

VOICE = "en-US-GuyNeural"   # deep, neutral, professional

NARRATION = [
    (
        "slide_01.mp3",
        "Welcome to Alpacaruns — a multi-agent paper-trading bot running live, "
        "twenty-four seven, on an UpCloud server. This demo console is wired "
        "directly into the bot's JSONL journal and Alpaca's account endpoint, "
        "so every number you see is real.",
    ),
    (
        "slide_02.mp3",
        "The Live view shows the bot's current status, equity, day and total "
        "P-and-L, and the drawdown halts. The equity curve updates every five "
        "seconds via the dashboard API. Right now the bot is running with a "
        "twenty-five symbol watchlist, paper-trading only.",
    ),
    (
        "slide_03.mp3",
        "Trades opens the full journal. Every fill the bot logs to data-slash-"
        "trades-dot-jsonl appears here with its decision path, confidence, "
        "and factor scores. Click any trade and the side panel walks you "
        "through exactly why it was placed — the agent's recommendation, "
        "the ensemble vote, the risk checks that passed, and the final "
        "order sent to Alpaca.",
    ),
    (
        "slide_04.mp3",
        "The Brain view shows what the agent is doing right now: open "
        "positions with their current P-and-L and the latest signal for "
        "each, plus a live feed of recent decision cycles — including the "
        "no-trade cycles, so you can see the bot thinking, not just acting.",
    ),
    (
        "slide_05.mp3",
        "Controls keeps the demo safe. You can pause new trades, resume, "
        "or run a single decision cycle on demand. No raw API keys are ever "
        "exposed — every order still flows through the same Go-coded risk "
        "validator the automated paths use.",
    ),
]


async def synth() -> None:
    for fname, text in NARRATION:
        out = OUT / fname
        if out.exists() and out.stat().st_size > 1000:
            print(f"  exists: {out.name}")
            continue
        comm = edge_tts.Communicate(text, VOICE)
        await comm.save(str(out))
        size = out.stat().st_size
        print(f"  wrote {out.name} ({size//1024} KB)")


def main() -> None:
    asyncio.run(synth())


if __name__ == "__main__":
    main()
