-- Migration 000004: full-text search support for listings.
-- Adds a GIN index over a tsvector built from title + description so the
-- GET /v1/listings/search endpoint can run plainto_tsquery matches efficiently.
--
-- NO FOREIGN KEY constraints platform-wide (CONVENTIONS §11 / CLAUDE.md #9).
-- We use the 'simple' text-search configuration intentionally: the marketplace
-- carries mixed-language (zh/en) content and 'simple' avoids language-specific
-- stemming that would silently drop CJK / non-English tokens. coalesce() guards
-- against NULL columns (description defaults to '' but title is also concatenated
-- defensively). The expression is IMMUTABLE, which is required for an expression
-- index — 'simple' + coalesce satisfy that.

CREATE INDEX listings_fts_idx
    ON listings
    USING gin (to_tsvector('simple', coalesce(title, '') || ' ' || coalesce(description, '')))
    WHERE deleted_at IS NULL;
