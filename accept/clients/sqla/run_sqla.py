"""SQLAlchemy 2.0 + psycopg2 against STASK and PostgreSQL 16.
   python3 -I run_sqla.py <host:port> <user> <password>
Prints one line per operation:  OK|FAIL <op> :: <result | SQLSTATE: message | [SQL that failed]>  (deterministic; diff two targets)."""
import sys, datetime, decimal, uuid, traceback
import sqlalchemy as sa
from sqlalchemy import (Table, Column, Integer, BigInteger, String, Numeric, Boolean, DateTime, LargeBinary, ForeignKey, Index,
                        UniqueConstraint, CheckConstraint, MetaData, select, func, text, inspect, insert, update, delete, Uuid, Sequence)
from sqlalchemy.dialects.postgresql import ARRAY, JSONB
from sqlalchemy.orm import DeclarativeBase, Mapped, mapped_column, relationship, Session, selectinload, joinedload

hostport, user, pw = sys.argv[1:4]
eng = sa.create_engine(f"postgresql+psycopg2://{user}:{pw}@{hostport}/stask?sslmode=disable", future=True)
cur_sql = []
@sa.event.listens_for(eng, "before_cursor_execute")
def _cap(conn, cursor, statement, params, context, executemany):
    cur_sql.append(" ".join(statement.split()))
ok = fails = 0
def op(name, fn):
    global ok, fails
    del cur_sql[:]
    try:
        r = fn(); ok += 1
        print(f"OK   {name} :: {r}")
    except Exception as e:
        fails += 1
        orig = getattr(e, "orig", e)
        st = getattr(orig, "pgcode", None) or type(e).__name__
        msg = " ".join(str(orig).split())[:230]
        last = cur_sql[-1][:420] if cur_sql else "-"
        print(f"FAIL {name} :: {st}: {msg}  [SQL: {last}]")
def norm(x):
    if isinstance(x, (list, tuple)): return [norm(i) for i in x]
    if isinstance(x, dict): return {k: norm(v) for k, v in sorted(x.items())}
    if isinstance(x, decimal.Decimal): return str(x)
    return x
def S(x): return str(norm(x))

with eng.begin() as c:
    for t in ["sa_item", "sa_order", "sa_cust", "sa_sales", "sa_types", "sa_orm_child", "sa_orm_parent", "sa_ck"]:
        c.execute(text(f"drop table if exists {t} cascade"))

op("connect/version", lambda: eng.connect().execute(text("select version()")).scalar().split(",")[0])
op("connect/dialect server_version_info", lambda: str(eng.dialect.server_version_info))

# ------------------------------------------------------------------ create_all
md = MetaData()
cust = Table("sa_cust", md, Column("id", Integer, primary_key=True), Column("name", String(50), nullable=False), Column("email", String(80), unique=True),
             Column("created", DateTime, server_default=func.now()))
order = Table("sa_order", md, Column("id", BigInteger, primary_key=True, autoincrement=True), Column("cust_id", Integer, ForeignKey("sa_cust.id"), nullable=False),
              Column("total", Numeric(12, 2)), Column("paid", Boolean, server_default=text("false")), Column("placed", DateTime(timezone=True)),
              Index("ix_sa_order_cust", "cust_id"), CheckConstraint("total >= 0", name="ck_sa_order_total"))
types = Table("sa_types", md, Column("id", Uuid, primary_key=True), Column("blob", LargeBinary), Column("ia", ARRAY(Integer)), Column("js", JSONB), Column("ts", DateTime), Column("n", Numeric(19, 2)))
op("ddl/create_all (serial pk, fk, unique, index, check, uuid pk, bytea, int[], jsonb)", lambda: (md.create_all(eng), "ok")[1])
op("ddl/create_all again (checkfirst via has_table)", lambda: (md.create_all(eng), "ok")[1])
def create_nochk():
    made = []
    for t in md.sorted_tables:
        try: t.create(eng, checkfirst=False); made.append(t.name)
        except sa.exc.ProgrammingError as e:
            if getattr(e.orig, "pgcode", "") != "42P07": raise
    return "created " + S(sorted(made))
op("ddl/WORKAROUND create each table with checkfirst=False", create_nochk)
op("ddl/Index on Numeric column", lambda: (Index("ix_sa_types_n", types.c.n).create(eng), "ok")[1])
def uuid_index():
    t2 = Table("sa_ck", md, Column("id", Integer, primary_key=True), Column("u", Uuid), Index("ix_sa_ck_u", "u"))
    t2.create(eng, checkfirst=False); return "ok"
op("ddl/Table with Index on Uuid column", uuid_index)
def enum_type():
    m = MetaData(); t = Table("sa_enum", m, Column("id", Integer, primary_key=True), Column("kind", sa.Enum("a", "b", name="sa_kind")))
    try: t.create(eng, checkfirst=False); return "ok"
    finally:
        for q in ("drop table if exists sa_enum", "drop type if exists sa_kind"):
            try:
                with eng.begin() as c: c.execute(text(q))
            except Exception: pass
op("ddl/Enum column (CREATE TYPE ... AS ENUM)", enum_type)
def seq():
    m = MetaData(); s = Sequence("sa_seq_x", metadata=m); t = Table("sa_seqt", m, Column("id", Integer, s, primary_key=True)); m.create_all(eng, checkfirst=False)
    with eng.begin() as c: v = c.execute(insert(t).returning(t.c.id)).scalar()
    m.drop_all(eng, checkfirst=False); return f"id={v}"
op("ddl/Sequence + insert returning + drop_all (checkfirst=False)", seq)
def ident():
    m = MetaData(); t = Table("sa_ident", m, Column("id", Integer, sa.Identity(start=5), primary_key=True), Column("x", String(5))); m.create_all(eng, checkfirst=False)
    with eng.begin() as c: v = c.execute(insert(t).values(x="a").returning(t.c.id)).scalar()
    m.drop_all(eng, checkfirst=False); return f"id={v}"
op("ddl/Identity column (checkfirst=False)", ident)

# ------------------------------------------------------------------ reflection
insp = inspect(eng)
op("inspect/get_schema_names has public", lambda: "public" in insp.get_schema_names())
op("inspect/get_table_names", lambda: S([t for t in insp.get_table_names() if t.startswith("sa_")]))
op("inspect/has_table sa_cust / nope", lambda: f"{insp.has_table('sa_cust')} {insp.has_table('nope_zz')}")
op("inspect/get_view_names", lambda: S(insp.get_view_names()))
op("inspect/get_columns sa_order", lambda: S([(c['name'], str(c['type']), c['nullable'], str(c['default']), c.get('autoincrement')) for c in insp.get_columns("sa_order")]))
op("inspect/get_columns sa_types", lambda: S([(c['name'], str(c['type']), c['nullable']) for c in insp.get_columns("sa_types")]))
op("inspect/get_pk_constraint sa_cust", lambda: S(insp.get_pk_constraint("sa_cust")))
op("inspect/get_foreign_keys sa_order", lambda: S(insp.get_foreign_keys("sa_order")))
op("inspect/get_indexes sa_order", lambda: S(insp.get_indexes("sa_order")))
op("inspect/get_unique_constraints sa_cust", lambda: S(insp.get_unique_constraints("sa_cust")))
op("inspect/get_check_constraints sa_order", lambda: S(insp.get_check_constraints("sa_order")))
op("inspect/get_table_comment", lambda: S(insp.get_table_comment("sa_cust")))
op("inspect/get_sequence_names", lambda: S(insp.get_sequence_names()))
op("inspect/get_multi_columns (2.0 bulk)", lambda: S(sorted((k[1], len(v)) for k, v in insp.get_multi_columns(filter_names=["sa_cust", "sa_order"]).items())))
op("inspect/get_multi_pk_constraint", lambda: S(sorted((k[1], v['constrained_columns']) for k, v in insp.get_multi_pk_constraint(filter_names=["sa_cust", "sa_order"]).items())))
op("inspect/get_multi_foreign_keys", lambda: S(sorted((k[1], len(v)) for k, v in insp.get_multi_foreign_keys(filter_names=["sa_cust", "sa_order"]).items())))
op("inspect/get_multi_indexes", lambda: S(sorted((k[1], len(v)) for k, v in insp.get_multi_indexes(filter_names=["sa_cust", "sa_order"]).items())))
op("reflect/MetaData.reflect(only=[...])", lambda: S(sorted((lambda m: (m.reflect(eng, only=["sa_cust", "sa_order"]), m.tables.keys())[1])(MetaData()))))
op("reflect/Table(autoload_with) + fk resolution", lambda: S(sorted(c.name for c in Table("sa_order", MetaData(), autoload_with=eng).c)))
op("reflect/MetaData.reflect() whole schema", lambda: str(len((lambda m: (m.reflect(eng), m.tables)[1])(MetaData())) >= 4))
# the exact reflection statement shape SQLAlchemy 2.0 builds ( pg_catalog.pg_class.relname ... ), run raw, then pieces of it
RAW = {
 "pg_class.relname select (get_table_names shape)": "SELECT pg_catalog.pg_class.relname FROM pg_catalog.pg_class JOIN pg_catalog.pg_namespace ON pg_catalog.pg_namespace.oid = pg_catalog.pg_class.relnamespace WHERE pg_catalog.pg_class.relkind = ANY (ARRAY['r', 'p']) AND pg_catalog.pg_table_is_visible(pg_catalog.pg_class.oid) AND pg_catalog.pg_namespace.nspname != 'pg_catalog'",
 "pg_class.relname with schema filter": "SELECT pg_catalog.pg_class.relname FROM pg_catalog.pg_class JOIN pg_catalog.pg_namespace ON pg_catalog.pg_namespace.oid = pg_catalog.pg_class.relnamespace WHERE pg_catalog.pg_class.relkind = ANY (ARRAY['r', 'p']) AND pg_catalog.pg_namespace.nspname = 'public'",
 "has_table shape (to_regclass-free)": "SELECT pg_catalog.pg_class.relname FROM pg_catalog.pg_class JOIN pg_catalog.pg_namespace ON pg_catalog.pg_namespace.oid = pg_catalog.pg_class.relnamespace WHERE pg_catalog.pg_class.relname = 'sa_cust' AND pg_catalog.pg_class.relkind = ANY (ARRAY['r', 'p', 'f', 'v', 'm']) AND pg_catalog.pg_table_is_visible(pg_catalog.pg_class.oid) AND pg_catalog.pg_namespace.nspname != 'pg_catalog'",
 "bound-param shape ANY(%(relkinds)s)": ("SELECT pg_catalog.pg_class.relname FROM pg_catalog.pg_class WHERE pg_catalog.pg_class.relkind = ANY (:k)", {"k": ["r", "p"]}),
 "format_type + pg_attribute + attrdef": "SELECT a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod), (SELECT pg_catalog.pg_get_expr(d.adbin, d.adrelid) FROM pg_catalog.pg_attrdef d WHERE d.adrelid = a.attrelid AND d.adnum = a.attnum AND a.atthasdef) AS DEFAULT, a.attnotnull, a.attrelid AS table_oid, a.attnum, a.attidentity, a.attgenerated FROM pg_catalog.pg_attribute a WHERE a.attrelid IN (SELECT oid FROM pg_class WHERE relname = 'sa_order') AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum",
 "pg_get_constraintdef (fk)": "SELECT r.conname, pg_catalog.pg_get_constraintdef(r.oid, true) AS condef, n.nspname AS conschema FROM pg_catalog.pg_constraint r JOIN pg_namespace n ON n.oid = r.connamespace WHERE r.conrelid = 'sa_order'::regclass AND r.contype = 'f' ORDER BY 1",
 "pg_index + pg_get_indexdef + unnest(indkey) (get_indexes)": "SELECT i.relname AS relname, ix.indisunique, ix.indexprs, a.attname, a.attnum, c.conrelid, ix.indkey, pg_catalog.pg_get_indexdef(ix.indexrelid) FROM pg_class t JOIN pg_index ix ON t.oid = ix.indrelid JOIN pg_class i ON i.oid = ix.indexrelid LEFT OUTER JOIN pg_attribute a ON t.oid = a.attrelid AND a.attnum = ANY(ix.indkey) LEFT OUTER JOIN pg_constraint c ON (ix.indrelid = c.conrelid AND ix.indexrelid = c.conindid AND c.contype IN ('p', 'u', 'x')) WHERE t.relname = 'sa_order' AND i.relname != 'sa_order_pkey' ORDER BY t.relname, i.relname",
 "pg_get_serial_sequence": "SELECT pg_get_serial_sequence('sa_order', 'id')",
 "information_schema.columns": "SELECT column_name, data_type, is_nullable FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'sa_order' ORDER BY ordinal_position",
 "information_schema.table_constraints + key_column_usage": "SELECT tc.constraint_name, tc.constraint_type, kcu.column_name FROM information_schema.table_constraints tc JOIN information_schema.key_column_usage kcu ON tc.constraint_name = kcu.constraint_name AND tc.table_schema = kcu.table_schema WHERE tc.table_name = 'sa_order' ORDER BY 1,3",
 "pg_catalog.pg_type enum labels (_load_enums)": "SELECT t.typname, t.typtype, pg_catalog.pg_type_is_visible(t.oid) AS visible, n.nspname FROM pg_catalog.pg_type t LEFT JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace WHERE t.typtype = 'e' ORDER BY 1",
 "pg_description (table comment)": "SELECT pg_catalog.obj_description(c.oid, 'pg_class') FROM pg_class c WHERE relname = 'sa_cust'",
 "current_schema / show": "SELECT current_schema(), current_database(), current_user, pg_catalog.pg_is_in_recovery()",
 "pg_extension / pg_roles": "SELECT extname FROM pg_extension ORDER BY 1",
 "pg_stat_activity / pg_database_size": "SELECT pg_database_size(current_database()) > 0",
 "pg_catalog.pg_get_userbyid / has_table_privilege": "SELECT has_table_privilege('sa_cust', 'SELECT')",
}
for k, q in RAW.items():
    params = {}
    if isinstance(q, tuple): q, params = q
    def run(q=q, params=params):
        with eng.connect() as c: rows = c.execute(text(q), params).all()
        return f"{len(rows)} rows " + S(sorted(map(tuple, rows)))[:160] if len(rows) < 12 else f"{len(rows)} rows"
    op("rawcat/" + k, run)

# ------------------------------------------------------------------ ORM CRUD
class Base(DeclarativeBase): pass
class Parent(Base):
    __tablename__ = "sa_orm_parent"
    id: Mapped[uuid.UUID] = mapped_column(Uuid, primary_key=True, default=uuid.uuid4)
    version: Mapped[int] = mapped_column(Integer, default=1)
    name: Mapped[str] = mapped_column(String(50))
    amount: Mapped[decimal.Decimal] = mapped_column(Numeric(19, 2), nullable=True)
    created: Mapped[datetime.datetime] = mapped_column(DateTime, nullable=True)
    children: Mapped[list["Child"]] = relationship(back_populates="parent", cascade="all, delete-orphan")
class Child(Base):
    __tablename__ = "sa_orm_child"
    id: Mapped[int] = mapped_column(Integer, primary_key=True)
    parent_id: Mapped[uuid.UUID] = mapped_column(Uuid, ForeignKey("sa_orm_parent.id"))
    label: Mapped[str] = mapped_column(String(20))
    parent: Mapped[Parent] = relationship(back_populates="children")
op("orm/create_all with uuid pk + fk to uuid pk", lambda: (Base.metadata.create_all(eng), "ok")[1])
# fall back: create the tables without the uuid FK so the CRUD tests can run, if the FK failed
def fallback_tables():
    with eng.begin() as c:
        c.execute(text("drop table if exists sa_orm_child cascade")); c.execute(text("drop table if exists sa_orm_parent cascade"))
        c.execute(text("create table sa_orm_parent(id uuid primary key, version int, name varchar(50), amount numeric(19,2), created timestamp)"))
        c.execute(text("create table sa_orm_child(id int primary key, parent_id uuid, label varchar(20))"))
    return "created without FK"
op("orm/fallback: tables without uuid FK", fallback_tables)
pid = uuid.UUID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
def orm_insert():
    with Session(eng) as s:
        p = Parent(id=pid, version=1, name="p1", amount=decimal.Decimal("12.50"), created=datetime.datetime(2026, 1, 2, 3, 4, 5))
        p.children = [Child(id=1, label="c1"), Child(id=2, label="c2")]
        s.add(p); s.commit(); return "committed"
op("orm/insert parent + children (cascade)", orm_insert)
def orm_query():
    with Session(eng) as s:
        p = s.get(Parent, pid); return f"{p.name} {p.amount} {p.created} {sorted(c.label for c in p.children)}"
op("orm/get by uuid pk + lazy load children", orm_query)
def orm_update():
    with Session(eng) as s:
        p = s.get(Parent, pid); p.name = "p1b"; p.amount = decimal.Decimal("99.99"); s.commit()
        s.refresh(p); return f"{p.name} {p.amount}"
op("orm/update + commit + refresh", orm_update)
def orm_select():
    with Session(eng) as s:
        rows = s.execute(select(Parent).options(selectinload(Parent.children)).where(Parent.name.like("p1%")).order_by(Parent.name).limit(10)).scalars().all()
        return f"{len(rows)} {len(rows[0].children)}"
op("orm/select + selectinload + like + limit", orm_select)
def orm_join():
    with Session(eng) as s:
        rows = s.execute(select(Parent.name, func.count(Child.id)).join(Child, Child.parent_id == Parent.id).group_by(Parent.name)).all(); return S([tuple(r) for r in rows])
op("orm/join on uuid + group by", orm_join)
def orm_joined():
    with Session(eng) as s:
        rows = s.execute(select(Parent).options(joinedload(Parent.children)).where(Parent.id == pid)).unique().scalars().all(); return str(len(rows[0].children))
op("orm/joinedload (left outer join)", orm_joined)
def orm_bulk():
    with Session(eng) as s:
        s.add_all([Parent(name=f"bulk{i}", amount=decimal.Decimal(i)) for i in range(200)]); s.commit()
        return str(s.scalar(select(func.count()).select_from(Parent).where(Parent.name.like("bulk%"))))
op("orm/add_all 200 (insertmanyvalues / executemany)", orm_bulk)
def core_returning():
    with eng.begin() as c:
        r = c.execute(insert(Parent.__table__).values(id=uuid.uuid4(), name="ret", version=1).returning(Parent.__table__.c.id, Parent.__table__.c.name)).one(); return r[1]
op("core/insert ... returning", core_returning)
def orm_bulk_returning():
    with eng.begin() as c:
        rows = c.execute(insert(Parent.__table__).returning(Parent.__table__.c.name), [{"id": uuid.uuid4(), "name": f"br{i}", "version": 1} for i in range(30)]).all(); return str(len(rows))
op("core/insert many ... returning (insertmanyvalues)", orm_bulk_returning)
def orm_delete():
    with Session(eng) as s:
        p = s.get(Parent, pid); s.delete(p); s.commit(); return f"left={s.scalar(select(func.count()).select_from(Child))}"
op("orm/delete parent (cascade children)", orm_delete)
def orm_forupdate():
    with Session(eng) as s:
        r = s.execute(select(Parent).where(Parent.name == "ret").with_for_update()).scalars().all(); s.commit(); return str(len(r))
op("orm/select for update", orm_forupdate)
def orm_rollback():
    with Session(eng) as s:
        s.add(Parent(name="rb")); s.flush(); s.rollback(); return str(s.scalar(select(func.count()).select_from(Parent).where(Parent.name == "rb")))
op("orm/flush + rollback", orm_rollback)
def orm_nested():
    with Session(eng) as s:
        s.add(Parent(name="n0")); s.flush()
        sp = s.begin_nested(); s.add(Parent(name="n1")); s.flush(); sp.rollback(); s.commit()
        return str(s.scalar(select(func.count()).select_from(Parent).where(Parent.name.in_(["n0", "n1"]))))
op("orm/begin_nested (SAVEPOINT) rollback", orm_nested)
def merge_upsert():
    from sqlalchemy.dialects.postgresql import insert as pginsert
    with eng.begin() as c:
        t = Parent.__table__; i = uuid.uuid4()
        c.execute(pginsert(t).values(id=i, name="up1", version=1).on_conflict_do_update(index_elements=["id"], set_={"name": "up2"}))
        c.execute(pginsert(t).values(id=i, name="up1", version=1).on_conflict_do_update(index_elements=["id"], set_={"name": "up2"}))
        return c.execute(select(t.c.name).where(t.c.id == i)).scalar()
op("core/pg insert ... on conflict do update (pk)", merge_upsert)
def types_roundtrip():
    with eng.begin() as c:
        i = uuid.uuid4()
        c.execute(insert(types).values(id=i, blob=b"\x00\x01\xff", ia=[1, 2, 3], js={"a": [1, 2], "b": "x"}, ts=datetime.datetime(2026, 5, 6, 7, 8, 9, 123456), n=decimal.Decimal("123.45")))
        r = c.execute(select(types.c.blob, types.c.ia, types.c.js, types.c.ts, types.c.n).where(types.c.id == i)).one(); return S(tuple(r))
op("core/types roundtrip: bytea,int[],jsonb,timestamp,numeric", types_roundtrip)
def jsonb_ops():
    with eng.connect() as c:
        return S(c.execute(select(types.c.js["b"].astext, types.c.js.has_key("a"), func.jsonb_array_length(types.c.js["a"]))).first())
op("core/jsonb ->> / ? / jsonb_array_length", jsonb_ops)
def array_ops():
    with eng.connect() as c:
        return S(c.execute(select(func.array_length(types.c.ia, 1), types.c.ia.contains([2]), (types.c.ia[1]))).first())
op("core/array_length, @>, subscript", array_ops)

# ------------------------------------------------------------------ Superset-style analytics
with eng.begin() as c:
    c.execute(text("drop table if exists sa_sales"))
    c.execute(text("create table sa_sales(id int primary key, day timestamp, region varchar(20), product varchar(20), qty int, price numeric(12,2), customer_id int, note text)"))
    rows = [{"id": i, "day": datetime.datetime(2026, 1, 1) + datetime.timedelta(hours=i * 7), "region": ["north", "south", "east", "west"][i % 4], "product": f"p{i % 7}",
             "qty": (i * 13) % 17 + 1, "price": decimal.Decimal((i * 37) % 1000) / 10, "customer_id": i % 53, "note": None if i % 5 == 0 else f"n{i}"} for i in range(3000)]
    c.execute(text("insert into sa_sales values (:id,:day,:region,:product,:qty,:price,:customer_id,:note)"), rows)
Q = {
 "count(*)": "select count(*) from sa_sales",
 "group by region sum/avg/count": "select region, sum(qty), avg(price), count(*) from sa_sales group by region order by region",
 "date_trunc month + group by 1": "select date_trunc('month', day) as __timestamp, sum(qty*price) as s from sa_sales group by date_trunc('month', day) order by __timestamp",
 "date_trunc week/day/hour/year/quarter": "select date_trunc('week', day), date_trunc('quarter', day), date_trunc('year', day), date_trunc('hour', day) from sa_sales where id = 100",
 "to_char variants": "select to_char(day,'YYYY-MM'), to_char(day,'DD/MM/YYYY HH24:MI:SS'), to_char(price,'FM999G999D00'), to_char(day,'Dy Mon') from sa_sales where id = 123",
 "count distinct": "select count(distinct customer_id), count(distinct product), count(distinct region || product) from sa_sales",
 "window row_number/rank/dense_rank": "select id, region, row_number() over (partition by region order by qty desc, id), rank() over (partition by region order by qty), dense_rank() over (order by qty) from sa_sales where id < 40 order by id",
 "window sum over (running) / lag / lead": "select id, sum(qty) over (partition by region order by id rows between unbounded preceding and current row), lag(qty) over (order by id), lead(qty,2) over (order by id) from sa_sales where id < 30 order by id",
 "window avg over () + percent": "select id, qty, avg(qty) over (), qty::float / sum(qty) over () from sa_sales where id < 10 order by id",
 "percentile_cont within group": "select region, percentile_cont(0.5) within group (order by price) from sa_sales group by region order by region",
 "filter (where ...)": "select region, count(*) filter (where qty > 8), sum(price) filter (where product = 'p1') from sa_sales group by region order by region",
 "case when buckets": "select case when qty < 5 then 'low' when qty < 12 then 'mid' else 'high' end b, count(*) from sa_sales group by 1 order by 1",
 "cte + subquery (virtual dataset)": "with t as (select region, product, sum(qty) q from sa_sales group by region, product) select region, max(q) from t group by region order by region",
 "virtual_table subquery alias": "select region, sum(q) from (select region, qty as q from sa_sales where qty > 3) as virtual_table group by region order by 2 desc limit 10",
 "having + order by aggregate desc limit": "select product, sum(qty) s from sa_sales group by product having sum(qty) > 100 order by s desc, product limit 5",
 "extract year/month/dow/epoch": "select extract(year from day), extract(month from day), extract(dow from day), extract(epoch from day) from sa_sales where id = 5",
 "coalesce/nullif/is null": "select count(*), count(note), sum(coalesce(length(note),0)), count(nullif(region,'north')) from sa_sales",
 "string_agg / array_agg": "select string_agg(distinct product, ',' order by product), array_agg(distinct region order by region) from sa_sales where id < 50",
 "generate_series calendar join": "select d::date, count(s.id) from generate_series('2026-01-01'::date, '2026-01-05'::date, '1 day') d left join sa_sales s on s.day::date = d::date group by 1 order by 1",
 "cast date / interval arithmetic": "select day::date, day + interval '1 day', day - interval '2 hours', (day::date - date '2026-01-01') from sa_sales where id = 9",
 "min/max timestamp, stddev, variance": "select min(day), max(day), stddev_samp(price)::numeric(10,3), var_pop(price)::numeric(10,3) from sa_sales",
 "ilike / regexp / like escape": "select count(*) from sa_sales where note ilike 'N1%' or note ~ '^n2[0-9]$' or product like 'p%\\_' escape '\\'",
 "in (subquery) / exists": "select count(*) from sa_sales a where customer_id in (select customer_id from sa_sales where qty = 17) and exists (select 1 from sa_sales b where b.id = a.id + 1)",
 "union all / intersect / except": "select region from sa_sales where id<4 union all select product from sa_sales where id<3 order by 1",
 "grouping sets / rollup": "select region, product, sum(qty) from sa_sales where id < 200 group by rollup(region, product) order by 1,2",
 "order by nulls last / distinct on": "select distinct on (region) region, id, qty from sa_sales order by region, qty desc, id",
 "limit 1000 offset": "select id from sa_sales order by id limit 5 offset 10",
 "lateral": "select r.region, x.id from (select distinct region from sa_sales) r, lateral (select id from sa_sales s where s.region = r.region order by id limit 1) x order by 1",
 "numeric rounding/ceil/floor/power": "select round(price, 1), round(price), ceil(price), floor(price), power(qty, 2), mod(qty, 5), abs(-qty) from sa_sales where id = 33",
 "concat / lpad / substring / split_part / upper": "select concat(region,'-',product), lpad(product,5,'0'), substring(note from 1 for 2), split_part(note,'1',2), upper(region), length(region) from sa_sales where id = 11",
 "SELECT 1 / show / set (superset connect)": "select 1",
 "bool_or/bool_and/every": "select bool_or(qty > 16), bool_and(qty > 0) from sa_sales",
 "approx: ntile + cume_dist": "select ntile(4) over (order by id), cume_dist() over (order by id) from sa_sales where id < 8 order by id",
 "to_timestamp / to_date / age": "select to_timestamp(0)::timestamp at time zone 'UTC', to_date('2026-02-03','YYYY-MM-DD')",
 "time zone convert": "select (day at time zone 'UTC') at time zone 'Asia/Ho_Chi_Minh' from sa_sales where id = 1",
 "now()/current_date type": "select now() is not null, current_date is not null",
}
for k, q in Q.items():
    def run(q=q):
        with eng.connect() as c: rows = c.execute(text(q)).all()
        out = S([tuple(r) for r in rows])
        return f"{len(rows)} rows " + (out if len(out) < 600 else out[:600] + "...")
    op("superset/" + k, run)
def superset_wire_style():
    with eng.connect() as c:
        c.execute(text("SET statement_timeout = '60s'")); return S(c.execute(text("show statement_timeout")).scalar())
op("superset/SET statement_timeout + show", superset_wire_style)
def superset_cursor():
    with eng.connect().execution_options(stream_results=True) as c:
        r = c.execute(text("select id, qty*price from sa_sales order by id")); n = 0; t = decimal.Decimal(0)
        for part in r.partitions(500): n += len(part); t += sum(x[1] for x in part)
        return f"{n} rows sum={t}"
op("superset/stream_results server-side cursor (DECLARE CURSOR) partitions(500)", superset_cursor)
def pandas_like():
    with eng.connect() as c:
        r = c.execute(text("select region, sum(qty) q from sa_sales group by region order by 1")); return f"{r.keys()} {[tuple(x) for x in r.fetchmany(2)]}"
op("superset/keys + fetchmany", pandas_like)
def pool_check():
    e2 = sa.create_engine(eng.url, pool_pre_ping=True, pool_size=3, max_overflow=0)
    with e2.connect() as a, e2.connect() as b: return str(a.execute(text("select 1")).scalar() + b.execute(text("select 2")).scalar())
op("engine/pool_pre_ping + 2 connections", pool_check)
def isolation():
    e3 = eng.execution_options(isolation_level="REPEATABLE READ")
    with e3.connect() as c: return str(c.execute(text("show transaction_isolation")).scalar())
op("engine/isolation_level=REPEATABLE READ", isolation)
def autocommit_iso():
    e3 = eng.execution_options(isolation_level="AUTOCOMMIT")
    with e3.connect() as c: c.execute(text("select 1")); return "ok"
op("engine/isolation_level=AUTOCOMMIT", autocommit_iso)

with eng.begin() as c:
    for t in ["sa_orm_child", "sa_orm_parent", "sa_sales", "sa_types", "sa_order", "sa_cust", "sa_ck"]: c.execute(text(f"drop table if exists {t} cascade"))
print(f"SUMMARY ok={ok} fail={fails}")
