-- Source schema of the shadow-mode test (SIM warehouse, in the spirit of cmd/simbench).
-- Covers: text/numeric/uuid/bytea/timestamp(tz)/jsonb/boolean/int types,
-- a single-column PK, a composite PK and a table without PK (duplicates allowed).
DROP TABLE IF EXISTS sims, orders, order_items, sim_history CASCADE;
DROP SEQUENCE IF EXISTS orders_seq;
CREATE SEQUENCE orders_seq;
CREATE TABLE sims (
  number text PRIMARY KEY, carrier text NOT NULL, status text NOT NULL, price numeric(12,2) NOT NULL,
  updated_at timestamptz NOT NULL, token uuid, meta jsonb, blob bytea, grade smallint);
CREATE TABLE orders (
  id bigint PRIMARY KEY, customer integer NOT NULL, status text NOT NULL, total numeric(14,2) NOT NULL,
  created_at timestamp NOT NULL, note varchar(40));
CREATE TABLE order_items (
  order_id bigint NOT NULL, line integer NOT NULL, number text NOT NULL, price numeric(12,2) NOT NULL,
  active boolean NOT NULL, PRIMARY KEY (order_id, line));
CREATE TABLE sim_history (
  number text NOT NULL, changed_at timestamptz NOT NULL, carrier text, status text, price numeric(12,2), source text);
