-- Suite group 12: text search and pattern matching. Syntax source: PostgreSQL 16 docs, chapter 12
-- (full text search), pg_trgm appendix F.35, section 9.7 (pattern matching).

-- ### 12_text_search/setup
DROP SCHEMA IF EXISTS s12 CASCADE;
CREATE SCHEMA s12;
SET search_path = s12;
CREATE TABLE doc (id int PRIMARY KEY, title text, body text, sim text);
INSERT INTO doc VALUES (1, 'The quick brown fox', 'jumps over the lazy dog', '0901234567'), (2, 'Postgres full text search', 'ranking, stemming and dictionaries for searching documents', '0912345678'), (3, 'Fast cars', 'the cars were running fast and the runners ran', '0987654321'), (4, 'Dog days', 'dogs are running in the park', '0901112233'), (5, 'Empty', '', '0981234567'), (6, 'Search engines', 'searched and searching; searches', '0771234567');

-- ### 12_text_search/fts
SET search_path = s12;
SELECT to_tsvector('english', 'The quick brown foxes jumped over the lazy dogs');
SELECT to_tsvector('simple', 'The quick brown foxes');
SELECT to_tsquery('english', 'quick & fox'), to_tsquery('english', 'quick | !dog'), plainto_tsquery('english', 'quick brown fox'), phraseto_tsquery('english', 'quick brown'), websearch_to_tsquery('english', '"quick brown" -dog or cat');
SELECT to_tsquery('english', 'fox:*'), to_tsquery('english', 'quick <-> brown'), to_tsquery('english', 'a <2> b');
SELECT to_tsvector('english', 'running runners ran') @@ to_tsquery('english', 'run');
SELECT id FROM doc WHERE to_tsvector('english', title || ' ' || body) @@ to_tsquery('english', 'run') ORDER BY id;
SELECT id FROM doc WHERE to_tsvector('english', body) @@ plainto_tsquery('english', 'dog') ORDER BY id;
SELECT id FROM doc WHERE to_tsvector('english', title) @@ to_tsquery('english', 'search:*') ORDER BY id;
SELECT id FROM doc WHERE to_tsvector('english', body) @@ to_tsquery('english', 'run & !dog') ORDER BY id;
SELECT id FROM doc WHERE to_tsvector('english', body) @@ websearch_to_tsquery('english', 'running -park') ORDER BY id;
SELECT id, ts_rank(to_tsvector('english', body), to_tsquery('english', 'run')) AS r FROM doc WHERE to_tsvector('english', body) @@ to_tsquery('english', 'run') ORDER BY r DESC, id;
SELECT id, ts_rank_cd(to_tsvector('english', body), to_tsquery('english', 'run & fast')) FROM doc WHERE id = 3;
SELECT ts_headline('english', 'The quick brown fox jumps over the lazy dog', to_tsquery('english', 'fox & dog'));
SELECT ts_headline('english', body, to_tsquery('english', 'search'), 'StartSel=<b>, StopSel=</b>') FROM doc WHERE id = 6;
SELECT setweight(to_tsvector('english', 'fox'), 'A') || setweight(to_tsvector('english', 'dog'), 'B');
SELECT strip(to_tsvector('english', 'fox dog')), length(to_tsvector('english', 'fox dog fox')), numnode(to_tsquery('a & b | c'));
SELECT ts_lexize('english_stem', 'running'), ts_lexize('simple', 'Running');
SELECT * FROM ts_debug('english', 'The foxes') ORDER BY 1, 2, 3, 4;
SELECT * FROM ts_parse('default', 'a b') ORDER BY 1, 2;
SELECT cfgname::text FROM pg_ts_config WHERE cfgname IN ('english', 'simple', 'german') ORDER BY 1;
SHOW default_text_search_config;
CREATE TABLE tsd (id int PRIMARY KEY, body text, tsv tsvector GENERATED ALWAYS AS (to_tsvector('english', body)) STORED);
INSERT INTO tsd SELECT id, title || ' ' || body FROM doc;
CREATE INDEX tsd_gin ON tsd USING gin (tsv);
SELECT id FROM tsd WHERE tsv @@ to_tsquery('english', 'search') ORDER BY id;
SELECT id FROM tsd WHERE tsv @@ to_tsquery('english', 'quick <-> brown') ORDER BY id;
UPDATE tsd SET body = 'completely different words' WHERE id = 1;
SELECT id FROM tsd WHERE tsv @@ to_tsquery('english', 'quick') ORDER BY id;
SELECT to_tsquery('english', 'a &');
SELECT to_tsquery('english', '(a');
SELECT 'a b'::tsvector, 'a:1 b:2'::tsvector, $$'a b':1$$::tsvector, 'a:1A'::tsvector;
SELECT 'a & b'::tsquery, 'a'::tsquery || 'b'::tsquery, !!'a'::tsquery, 'a & b'::tsquery @> 'a'::tsquery;

-- ### 12_text_search/trigram-and-patterns
SET search_path = s12;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
SELECT show_trgm('word'), similarity('word', 'words'), 'word' % 'words', word_similarity('wor', 'word'), strict_word_similarity('wor', 'word');
CREATE INDEX doc_sim_trgm ON doc USING gin (sim gin_trgm_ops);
SELECT id FROM doc WHERE sim LIKE '090%' ORDER BY id;
SELECT id FROM doc WHERE sim LIKE '%567' ORDER BY id;
SELECT id FROM doc WHERE sim LIKE '%123%' ORDER BY id;
SELECT id FROM doc WHERE sim LIKE '0_1%' ORDER BY id;
SELECT id FROM doc WHERE sim ILIKE '%1234%' ORDER BY id;
SELECT id FROM doc WHERE sim ~ '^09[0-9]{2}1' ORDER BY id;
SELECT id FROM doc WHERE sim NOT LIKE '09%' ORDER BY id;
SELECT id FROM doc WHERE right(sim, 4) = '4567' ORDER BY id;
SELECT id FROM doc WHERE left(sim, 3) = '098' ORDER BY id;
SELECT id FROM doc WHERE reverse(sim) LIKE '76%' ORDER BY id;
SELECT id FROM doc WHERE starts_with(sim, '091') ORDER BY id;
SELECT id FROM doc WHERE position('23' in sim) > 0 ORDER BY id;
SELECT id FROM doc WHERE strpos(sim, '12') = 4 ORDER BY id;
SELECT id FROM doc WHERE title ILIKE '%DOG%' OR body ILIKE '%dog%' ORDER BY id;
SELECT id FROM doc WHERE title ~* '^(the|fast)' ORDER BY id;
SELECT id FROM doc WHERE body LIKE '%run%' AND body NOT LIKE '%runners%' ORDER BY id;
SELECT id, similarity(title, 'Postgres search') AS s FROM doc WHERE title % 'Postgres search' ORDER BY s DESC, id;
SELECT id, title <-> 'quick fox' AS dist FROM doc ORDER BY dist, id LIMIT 3;
SET pg_trgm.similarity_threshold = 0.5;
SHOW pg_trgm.similarity_threshold;
SELECT id FROM doc WHERE sim % '0901234568' ORDER BY id;
SELECT id FROM doc WHERE sim ~~ '090%' ORDER BY id;
SELECT id, sim FROM doc WHERE sim LIKE ANY (ARRAY['090%', '098%']) ORDER BY id;
SELECT id, sim FROM doc WHERE sim LIKE ALL (ARRAY['09%', '%7']) ORDER BY id;
SELECT id FROM doc WHERE sim SIMILAR TO '(090|091)%' ORDER BY id;

-- ### 12_text_search/teardown
DROP SCHEMA s12 CASCADE;
