MinifluxNg
==========

MinifluxNg is a fork of [Miniflux](https://github.com/miniflux/v2) — a minimalist and opinionated feed reader — with additional features for AI-powered reading, web scraping, and JavaScript rendering.

Regularly synced with upstream Miniflux to stay current with bug fixes and improvements.

Fork Features
-------------

### AI Summary & Digest

- Per-user OpenAI-compatible API configuration for automatic article summarization.
- AI Digest page with scoring, one-click page summary generation, and batch mark-as-read.
- Language-aware: summaries generated in the user's preferred language.
- Backfill support: batch-process existing articles with start/stop controls.

### Web Scraper Engine

- Subscribe to any website without RSS — extract articles using CSS selectors (HTML) or gjson paths (JSON).
- Pagination support for both HTML (regex) and JSON (gjson path) sources.
- Feed source type selector: choose between RSS/Atom/JSON Feed or Web Scraper per subscription.

### JavaScript Rendering (Obscura)

- Optional stealth headless browser rendering via [Obscura](https://github.com/h4ckf0r0day/obscura) for JavaScript-heavy websites.
- Lightweight V8-based rendering with Chrome DevTools Protocol automation.
- Two-stage content extraction: Obscura renders the page, then [Defuddle](https://github.com/kepano/defuddle) extracts the article content server-side.
- Works with both RSS feeds (fetch original content) and Web Scraper feeds (render listing pages + article detail pages).
- Per-feed proxy support (`--proxy`); render failures are surfaced instead of falling back to standard HTTP fetching.
- The Docker image pins Obscura v0.2.1's `no-render-stealth` build; Rod remains on the latest stable v0.116.2.
- Private/local network access is blocked by default. Set `OBSCURA_ALLOW_PRIVATE_NETWORKS=1` only for feeds that intentionally require it.

Upstream Features
-----------------

All features from upstream Miniflux are included. See the [official documentation](https://miniflux.app/docs/) for details on:

- Feed formats (Atom, RSS, JSON Feed), OPML import/export
- Privacy & security (tracker removal, CSP, media proxy)
- 25+ third-party integrations
- Fever & Google Reader API compatibility
- Passkeys, OAuth2, OpenID Connect authentication
- Keyboard shortcuts, themes, responsive design
- Single Go binary, PostgreSQL only, minimal resource usage

Documentation
-------------

- Upstream docs: <https://miniflux.app/docs/>
- [Configuration](https://miniflux.app/docs/configuration.html)
- [API Reference](https://miniflux.app/docs/api.html)
- [Rewrite and Scraper Rules](https://miniflux.app/docs/rules.html)

Credits
-------

- Upstream: [Miniflux](https://github.com/miniflux/v2) by Frédéric Guillot
- Distributed under Apache 2.0 License
