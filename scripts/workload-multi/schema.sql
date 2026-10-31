-- Validation workload "multi": SIM warehouse + orders + stock + history (docs/30).
-- PostgreSQL syntax that STASK also accepts. Run: psql -v ON_ERROR_STOP=1 -f schema.sql
DROP TABLE IF EXISTS history, order_items, orders, stock, sims, stores, carriers, customers CASCADE;
CREATE TABLE carriers  (id int PRIMARY KEY, code text NOT NULL, name text NOT NULL);
CREATE TABLE stores    (id int PRIMARY KEY, name text NOT NULL, region text NOT NULL, rate int NOT NULL);
CREATE TABLE customers (id int PRIMARY KEY, segment text NOT NULL, city text NOT NULL);
CREATE TABLE sims (
  number     text PRIMARY KEY,           -- '09' + 9 digits, high cardinality text key
  carrier_id int  NOT NULL,              -- 1..3, ratio 30/50/20 in the data
  store_id   int  NOT NULL,              -- 1..40
  category   text NOT NULL,              -- 5 values
  price      int  NOT NULL,              -- 500000 .. 4500000
  store_rate int  NOT NULL,              -- 5..10 priority weight
  status     text NOT NULL,              -- available / reserved / sold
  updated_at timestamptz NOT NULL
);
CREATE TABLE stock (
  store_id int NOT NULL, category text NOT NULL,
  qty int NOT NULL, reserved int NOT NULL,
  PRIMARY KEY (store_id, category)
);
CREATE TABLE orders (
  id bigint PRIMARY KEY, customer_id int NOT NULL, store_id int NOT NULL,
  status text NOT NULL,                  -- open / paid / shipped / cancelled
  created_at timestamptz NOT NULL, total bigint NOT NULL
);
CREATE TABLE order_items (
  id bigint PRIMARY KEY, order_id bigint NOT NULL, sim_number text NOT NULL, price int NOT NULL
);
CREATE TABLE history (
  id bigint PRIMARY KEY, sim_number text NOT NULL, changed_at timestamptz NOT NULL,
  field text NOT NULL,                   -- price / status / store
  old_value text, new_value text
);
