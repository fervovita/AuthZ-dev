-- Identifiers are ASCII, so "C" compares bytes and no database locale reorders them.
CREATE TABLE tuples (
    resource_type    text COLLATE "C" NOT NULL,
    resource_id      text COLLATE "C" NOT NULL,
    relation         text COLLATE "C" NOT NULL,
    subject_type     text COLLATE "C" NOT NULL,
    subject_id       text COLLATE "C" NOT NULL, -- "*" names every subject of subject_type
    subject_relation text COLLATE "C" NOT NULL, -- "" names the subject itself
    PRIMARY KEY (resource_type, resource_id, relation, subject_type, subject_id, subject_relation)
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

-- Every schema written, in the language's source. xid orders each version among the tuple writes.
CREATE TABLE schema_versions (
    version bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    xid     xid8 NOT NULL DEFAULT pg_current_xact_id(),
    source  text NOT NULL
);
