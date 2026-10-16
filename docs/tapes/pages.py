# Takes docs/assets/team-*.png and admin-*.png, the hub's pages in the README and docs/self-hosting.md, from the
# pages' demo mode, in the dark and the light scheme. With pnpm demo running in web/, from the repository root:
#
#   uv run --with playwright python3 docs/tapes/pages.py
#
# It drives the Chrome you have installed; Playwright's own browsers aren't needed.
from playwright.sync_api import sync_playwright

pages = {"team": "/team", "admin": "/admin"}
with sync_playwright() as p:
    browser = p.chromium.launch(channel="chrome")
    for scheme in ("dark", "light"):
        page = browser.new_page(viewport={"width": 1280, "height": 860}, device_scale_factor=2, color_scheme=scheme)
        for name, path in pages.items():
            page.goto("http://localhost:4321" + path, wait_until="domcontentloaded")
            page.wait_for_timeout(2500)  # the made-up team arrives over the event stream
            page.screenshot(path=f"docs/assets/{name}-{scheme}.png")
        page.close()
    browser.close()
