-- +goose Up

-- The firewall the panel manages, as declarative rows.
--
-- The nftables ruleset is rendered from this table in full on every change,
-- never patched in place. Parsing `nft list` back into rules was the largest
-- source of bugs in the previous panel; here the database is the truth and
-- the kernel is a projection of it.
CREATE TABLE firewall_rules (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    -- port  : open a port to everyone (or to a source, when address is set)
    -- block : refuse an address or range outright
    -- allow : accept an address or range before the blocklist is consulted
    kind       TEXT    NOT NULL,
    protocol   TEXT    NOT NULL DEFAULT 'tcp',   -- tcp | udp | both
    port_from  INTEGER NOT NULL DEFAULT 0,
    port_to    INTEGER NOT NULL DEFAULT 0,
    -- An address or CIDR. Empty on a port rule means "from anywhere".
    address    TEXT    NOT NULL DEFAULT '',
    comment    TEXT    NOT NULL DEFAULT '',
    enabled    INTEGER NOT NULL DEFAULT 1,
    -- A rule the installer created, which the panel may rewrite but a
    -- customer should not casually delete.
    managed    INTEGER NOT NULL DEFAULT 0,
    created_at TEXT    NOT NULL
);
CREATE INDEX idx_firewall_kind ON firewall_rules(kind);

-- Blocklists fetched from a URL, one address or CIDR per line.
CREATE TABLE firewall_sources (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    url           TEXT    NOT NULL UNIQUE,
    description   TEXT    NOT NULL DEFAULT '',
    enabled       INTEGER NOT NULL DEFAULT 1,
    -- How often to refetch, in hours.
    interval_hours INTEGER NOT NULL DEFAULT 24,
    last_fetch_at TEXT    NOT NULL DEFAULT '',
    last_error    TEXT    NOT NULL DEFAULT '',
    entry_count   INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT    NOT NULL
);

-- The addresses a fetch produced. Kept in the database rather than only in
-- the kernel so the ruleset can be rebuilt after a reboot without waiting
-- for every source to be reachable again.
CREATE TABLE firewall_source_entries (
    source_id INTEGER NOT NULL REFERENCES firewall_sources(id) ON DELETE CASCADE,
    address   TEXT    NOT NULL,
    PRIMARY KEY (source_id, address)
);

-- +goose Down
DROP TABLE firewall_source_entries;
DROP TABLE firewall_sources;
DROP INDEX idx_firewall_kind;
DROP TABLE firewall_rules;
