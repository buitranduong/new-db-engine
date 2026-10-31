// Npgsql smoke: connect, SELECT 1, prepared statement, COPY (text). dotnet run
// Env: PGHOST PGPORT PGUSER PGPASSWORD PGDATABASE.
using Npgsql;

static string E(string k, string d) => string.IsNullOrEmpty(Environment.GetEnvironmentVariable(k)) ? d : Environment.GetEnvironmentVariable(k)!;

var cs = $"Host={E("PGHOST", "127.0.0.1")};Port={E("PGPORT", "5432")};Username={E("PGUSER", "postgres")};" +
         $"Password={E("PGPASSWORD", "")};Database={E("PGDATABASE", "postgres")};SSL Mode={E("PGSSLMODE", "Disable")}";
try
{
    await using var conn = new NpgsqlConnection(cs);
    await conn.OpenAsync();
    await using (var cmd = new NpgsqlCommand("SELECT 1", conn))
        if ((int)(await cmd.ExecuteScalarAsync())! != 1) throw new Exception("select 1");
    await new NpgsqlCommand("DROP TABLE IF EXISTS suite_smoke", conn).ExecuteNonQueryAsync();
    await new NpgsqlCommand("CREATE TABLE suite_smoke (id int PRIMARY KEY, v text)", conn).ExecuteNonQueryAsync();
    try
    {
        await using (var ins = new NpgsqlCommand("INSERT INTO suite_smoke VALUES ($1, $2)", conn)
                     { Parameters = { new() { Value = 1 }, new() { Value = "prepared" } } })
        {
            await ins.PrepareAsync();   // server-side prepared statement
            await ins.ExecuteNonQueryAsync();
        }
        await using (var w = await conn.BeginTextImportAsync("COPY suite_smoke FROM STDIN"))
            await w.WriteAsync("2\tcopied\n3\tcopied too\n");
        var lines = 0;
        await using (var r = await conn.BeginTextExportAsync("COPY suite_smoke TO STDOUT"))
            while (await r.ReadLineAsync() != null) lines++;
        if (lines != 3) throw new Exception($"copy to returned {lines} lines");
        await using var cnt = new NpgsqlCommand("SELECT count(*) FROM suite_smoke WHERE v LIKE $1", conn)
                              { Parameters = { new() { Value = "copied%" } } };
        if ((long)(await cnt.ExecuteScalarAsync())! != 2) throw new Exception("param query");
    }
    finally { await new NpgsqlCommand("DROP TABLE IF EXISTS suite_smoke", conn).ExecuteNonQueryAsync(); }
    Console.WriteLine("OK npgsql");
}
catch (Exception e) { Console.Error.WriteLine("FAIL npgsql: " + e.Message); return 1; }
return 0;
