CREATE TABLE balances (
    id serial PRIMARY KEY,
    client_key text UNIQUE NOT NULL,
    private_key text NOT NULL,
    private_key_salt text NOT NULL,
    balance NUMERIC(40,10) NOT NULL
);
CREATE USER docker WITH PASSWORD 'prettystrongpassword';
GRANT ALL PRIVILEGES ON balances TO docker;