#!/usr/bin/env python3
"""Assemble the final video:
- For each slide, render a per-slide MP4 whose duration matches that slide's MP3 length
  (looping the static PNG as a video stream).
- Concatenate all per-slide videos.
- Mux with the full narration.mp3.

ffmpeg handles timing so the video ends naturally with the audio via -shortest.
"""
import os, subprocess

FFMPEG = r"C:/Users/HP/scoop/shims/ffmpeg.exe"
ROOT   = r"C:/Users/HP/Desktop/Alpacaruns/hackathon_video"
SLIDES = os.path.join(ROOT, "slides")
AUDIO  = os.path.join(ROOT, "slide_audio")
PER    = os.path.join(ROOT, "per_slide")
FINAL  = os.path.join(ROOT, "hackathon_pitch.mp4")
TARGET = r"C:/Users/HP/Desktop/Alpacaruns/hackathon_pitch.mp4"

os.makedirs(PER, exist_ok=True)

def probe_duration(path: str) -> float:
    # Prefer ffprobe (clean exit code), fall back to parsing ffmpeg's stderr
    try:
        out = subprocess.check_output(
            ["ffprobe", "-v", "error", "-show_entries", "format=duration",
             "-of", "default=noprint_wrappers=1:nokey=1", path],
            stderr=subprocess.STDOUT,
        ).decode().strip()
        return float(out)
    except Exception:
        pass
    res = subprocess.run([FFMPEG, "-hide_banner", "-i", path],
                          capture_output=True, text=True)
    import re
    m = re.search(r"Duration: (\d+):(\d+):(\d+\.\d+)", res.stderr + res.stdout)
    if not m:
        raise RuntimeError(f"could not probe duration of {path}")
    h, mi, s = m.groups()
    return int(h) * 3600 + int(mi) * 60 + float(s)

def render_slide_video(idx: int, png: str, mp3: str, out: str):
    dur = probe_duration(mp3)
    # loop the PNG as a still image for `dur` seconds at 30fps, 720p
    cmd = [
        FFMPEG, "-y",
        "-loop", "1",
        "-framerate", "30",
        "-i", png,
        "-i", mp3,
        "-t", f"{dur:.3f}",
        "-c:v", "libx264",
        "-pix_fmt", "yuv420p",
        "-preset", "veryfast",
        "-crf", "20",
        "-vf", "scale=1280:720",
        "-c:a", "aac",
        "-b:a", "128k",
        "-ar", "44100",
        "-ac", "2",
        "-shortest",
        "-movflags", "+faststart",
        out,
    ]
    res = subprocess.run(cmd, capture_output=True, text=True)
    if res.returncode != 0:
        print("FFMPEG STDERR:", res.stderr[-2000:])
        raise RuntimeError(f"failed rendering slide {idx}")
    print(f"  slide {idx}: {dur:.1f}s -> {os.path.basename(out)}")

def main():
    concat_inputs = []
    for i in range(1, 8):
        png = os.path.join(SLIDES, f"slide_{i:02d}.png")
        mp3 = os.path.join(AUDIO, f"slide_{i:02d}.mp3")
        out = os.path.join(PER, f"slide_{i:02d}.mp4")
        render_slide_video(i, png, mp3, out)
        concat_inputs.append(out)

    # concat all per-slide videos (audio+video) into one file
    concat_file = os.path.join(ROOT, "concat_videos.txt")
    with open(concat_file, "w", encoding="utf-8") as f:
        for p in concat_inputs:
            f.write(f"file '{p}'\n")

    cmd = [
        FFMPEG, "-y",
        "-f", "concat", "-safe", "0",
        "-i", concat_file,
        "-c", "copy",
        "-movflags", "+faststart",
        FINAL,
    ]
    res = subprocess.run(cmd, capture_output=True, text=True)
    if res.returncode != 0:
        print("FFMPEG STDERR:", res.stderr[-2000:])
        raise RuntimeError("concat failed")

    # copy to project root as final deliverable
    import shutil
    shutil.copy2(FINAL, TARGET)
    dur = probe_duration(FINAL)
    print(f"\nFINAL: {TARGET}  duration={dur:.1f}s ({dur/60:.2f} min)  size={os.path.getsize(TARGET)//1024} KiB")

if __name__ == "__main__":
    main()
