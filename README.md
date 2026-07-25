# omnius-cinemeta

Self-hosted metadata service for the Omnius server — a drop-in for the dead
`api.imdbapi.dev`. Serves that contract (`/titles/{id}`, `/titles/{id}/episodes`,
`/search/titles`) from Stremio Cinemeta (imdb-native, posters via metahub),
with in-memory caching. Stdlib-only Go.

Deploy: `bp deploy` (basepod) — runs at cinemeta.omnius.lol.
