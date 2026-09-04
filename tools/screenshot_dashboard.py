#!/usr/bin/env python
"""Take screenshots of all 5 dashboard pages for the slide deck.

Output: hackathon_video/screenshots_v2/{welcome,live,trades,brain,controls}.png
"""
from playwright.sync_api import sync_playwright
import os, time

OUT = os.path.join(os.path.dirname(__file__), '..', 'hackathon_video', 'screenshots_v2')
os.makedirs(OUT, exist_ok=True)

PAGES = [
    ('welcome',  'http://5.22.215.51/welcome'),
    ('live',     'http://5.22.215.51/live'),
    ('trades',   'http://5.22.215.51/trades'),
    ('brain',    'http://5.22.215.51/brain'),
    ('controls', 'http://5.22.215.51/controls'),
]

with sync_playwright() as p:
    browser = p.chromium.launch(headless=True, args=['--no-sandbox', '--disable-dev-shm-usage'])
    context = browser.new_context(viewport={'width': 1600, 'height': 1000})
    page = context.new_page()

    for name, url in PAGES:
        print(f'Navigating to {name}: {url}', flush=True)
        try:
            page.goto(url, timeout=30000, wait_until='networkidle')
        except Exception as e:
            print(f'  networkidle timeout: {e}', flush=True)
            page.goto(url, timeout=30000, wait_until='domcontentloaded')
        time.sleep(4)  # let SWR + recharts paint
        out = os.path.join(OUT, f'{name}.png')
        page.screenshot(path=out, full_page=True)
        size = os.path.getsize(out)
        print(f'  saved {out} ({size//1024} KB)', flush=True)

    browser.close()

print('all done')
