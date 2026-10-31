// Node pg smoke: connect, SELECT 1, named prepared statement, COPY.
// npm i pg pg-copy-streams ; env: PGHOST PGPORT PGUSER PGPASSWORD PGDATABASE
const { Client } = require('pg');
const { from: copyFrom, to: copyTo } = require('pg-copy-streams');
const { Readable, Writable } = require('stream');
const { pipeline } = require('stream/promises');

async function main() {
  const c = new Client({
    host: process.env.PGHOST || '127.0.0.1', port: +(process.env.PGPORT || 5432),
    user: process.env.PGUSER || 'postgres', password: process.env.PGPASSWORD || '',
    database: process.env.PGDATABASE || 'postgres', ssl: process.env.PGSSLMODE === 'require',
  });
  await c.connect();
  try {
    const one = await c.query('SELECT 1 AS one');
    if (one.rows[0].one !== 1) throw new Error('select 1');
    await c.query('DROP TABLE IF EXISTS suite_smoke');
    await c.query('CREATE TABLE suite_smoke (id int PRIMARY KEY, v text)');
    // a query with a name and values is a server-side prepared statement (extended protocol)
    await c.query({ name: 'smoke_ins', text: 'INSERT INTO suite_smoke VALUES ($1, $2)', values: [1, 'prepared'] });
    await pipeline(Readable.from(['2\tcopied\n3\tcopied too\n']), c.query(copyFrom('COPY suite_smoke FROM STDIN')));
    let out = '';
    await pipeline(c.query(copyTo('COPY suite_smoke TO STDOUT')),
      new Writable({ write(chunk, _e, cb) { out += chunk.toString(); cb(); } }));
    if (out.split('\n').length - 1 !== 3) throw new Error('copy to: ' + JSON.stringify(out));
    const n = await c.query('SELECT count(*)::int AS n FROM suite_smoke WHERE v LIKE $1', ['copied%']);
    if (n.rows[0].n !== 2) throw new Error('param query');
  } finally {
    await c.query('DROP TABLE IF EXISTS suite_smoke').catch(() => {});
    await c.end();
  }
  console.log('OK node-pg');
}
main().catch((e) => { console.error('FAIL node-pg:', e.message); process.exit(1); });
