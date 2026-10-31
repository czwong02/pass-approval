import postgres from "postgres";

const url = process.env.DATABASE_URL;
if (!url) throw new Error("DATABASE_URL is required");

export const sql = postgres(url, { ssl: "require", max: 1, prepare: false });

export type Db = ReturnType<typeof postgres>;
export type Query = postgres.Sql | postgres.TransactionSql;