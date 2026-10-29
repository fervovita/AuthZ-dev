-- Identifiers are ASCII, so "C" compares bytes and no database locale reorders them.
CREATE TABLE tuples (
    resource_type    text COLLATE "C" NOT NULL,
    resource_id      text COLLATE "C" NOT NULL,
    relation         text COLLATE "C" NOT NULL,
    subject_type     text COLLATE "C" NOT NULL,
    subject_id       text COLLATE "C" NOT NULL, -- "*" names every subject of subject_type
    subject_relation text COLLATE "C" NOT NULL, -- "" names the subject itself
    -- relation before resource_id keeps each relation's tuples in one key range.
    PRIMARY KEY (resource_type, relation, resource_id, subject_type, subject_id, subject_relation)
);

-- Which tuples each transaction touched, not what it did: a change goes out as the tuple's state now,
-- because replaying operations in xid order can put an earlier write last.
CREATE TABLE change_log (
    xid              xid8 NOT NULL DEFAULT pg_current_xact_id(),
    resource_type    text COLLATE "C" NOT NULL,
    resource_id      text COLLATE "C" NOT NULL,
    relation         text COLLATE "C" NOT NULL,
    subject_type     text COLLATE "C" NOT NULL,
    subject_id       text COLLATE "C" NOT NULL,
    subject_relation text COLLATE "C" NOT NULL
);

CREATE INDEX change_log_xid ON change_log (xid);

-- Where change_log may be cut.
-- The oldest mark is where the log starts.
CREATE TABLE log_marks (
    xid       xid8 PRIMARY KEY,
    marked_at timestamptz NOT NULL
);

-- Until Prune first cuts it, the log starts before every transaction.
INSERT INTO log_marks (xid, marked_at) VALUES ('0', '-infinity');

-- Every schema written, in the language's source. xid orders each version among the tuple writes.
CREATE TABLE schema_versions (
    version bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    xid     xid8 NOT NULL DEFAULT pg_current_xact_id(),
    source  text NOT NULL
);
