-- sql/install/postgres.global.sql
--
-- PostgreSQL global schema for Adept.
--
-- Notes
-- -----
-- • Uses TIMESTAMPTZ for audit fields.
-- • Uses ENUMs for stable status and provider values.
-- • Uses a shared trigger to keep updated_at current.

CREATE TYPE user_status AS ENUM ('Active', 'Blocked', 'Inactive', 'Locked');
CREATE TYPE oauth_provider AS ENUM ('Google', 'GitHub', 'Microsoft', 'Apple', 'Custom');
CREATE TYPE user_profile_status AS ENUM ('Active', 'Block', 'Inactive', 'Locked');
CREATE TYPE passkey_transport AS ENUM ('usb', 'nfc', 'ble', 'internal');

CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = CURRENT_TIMESTAMP;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TABLE site (
  id            BIGSERIAL PRIMARY KEY,
  host          VARCHAR(256)  NOT NULL UNIQUE,
  theme         VARCHAR(128)  NOT NULL DEFAULT 'base',
  locale        VARCHAR(16)   NOT NULL DEFAULT 'en_US',
  routing_mode  VARCHAR(6)    NOT NULL DEFAULT 'path',
  route_version INT           NOT NULL DEFAULT 0,
  preload       BOOLEAN       NOT NULL DEFAULT FALSE,
  suspended_at  TIMESTAMPTZ NULL,
  deleted_at    TIMESTAMPTZ NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO `site` (`host`) VALUES ('yaniz.dev');
INSERT INTO `site` (`host`) VALUES ('yaniz.int');

CREATE TRIGGER site_set_updated_at
BEFORE UPDATE ON site
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE OR REPLACE VIEW site_status_view AS
SELECT
    id,
    host,
    CASE
        WHEN deleted_at  IS NOT NULL THEN 'deleted'
        WHEN suspended_at IS NOT NULL THEN 'suspended'
        ELSE 'ok'
    END AS status
FROM site;

CREATE TABLE site_config (
  site_id  BIGINT NOT NULL,
  key      VARCHAR(64)  NOT NULL,
  value    TEXT         NOT NULL,

  PRIMARY KEY (site_id, key),
   CONSTRAINT fk_site_config_site
      FOREIGN KEY (site_id)
      REFERENCES site(id)
      ON DELETE CASCADE
);


-- ---------- core user record ----------
CREATE TABLE users (
  id              BIGSERIAL PRIMARY KEY,
  username        VARCHAR(128) NOT NULL,
  email           VARCHAR(256) NOT NULL,
  status          user_status NOT NULL DEFAULT 'Inactive',
  created_at      TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
  verified_at     TIMESTAMPTZ NULL,
  -- application-level bookkeeping
  UNIQUE (username),
  UNIQUE (email)
);

CREATE TRIGGER users_set_updated_at
BEFORE UPDATE ON users
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE password_credentials (
  id              BIGSERIAL PRIMARY KEY,
  user_id         BIGINT NOT NULL,
  password_hash   BYTEA        NOT NULL,                   -- Argon2id or bcrypt
  needs_rotation  BOOLEAN      NOT NULL DEFAULT FALSE,
  created_at      TIMESTAMPTZ  NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at      TIMESTAMPTZ  NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
  UNIQUE (user_id)
);

CREATE TRIGGER password_credentials_set_updated_at
BEFORE UPDATE ON password_credentials
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE oauth_identities (
  id                 BIGSERIAL PRIMARY KEY,
  user_id            BIGINT NOT NULL,
  provider           oauth_provider NOT NULL,
  provider_user_id   VARCHAR(256) NOT NULL,                  -- sub claim or user ID from IdP
  access_token_enc   BYTEA NULL,                              -- optional, encrypted at rest
  refresh_token_enc  BYTEA NULL,
  expires_at         TIMESTAMP NULL,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at         TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
  UNIQUE (provider, provider_user_id)
);

CREATE TRIGGER oauth_identities_set_updated_at
BEFORE UPDATE ON oauth_identities
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE passkey_credentials (
  id                 BIGSERIAL PRIMARY KEY,
  user_id            BIGINT NOT NULL,
  credential_id      BYTEA NOT NULL,                -- base64url decoded bytes
  public_key         BYTEA NOT NULL,                -- COSE key bytes
  sign_count         BIGINT NOT NULL DEFAULT 0,
  transports         passkey_transport[] NOT NULL,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
  last_used_at       TIMESTAMPTZ NULL,
  FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
  UNIQUE (credential_id)
);

DROP TABLE IF EXISTS user_profile;
CREATE TABLE user_profile (
  id               BIGSERIAL PRIMARY KEY,
  user_id          BIGINT NOT NULL,
  name_prefix      VARCHAR(40) DEFAULT NULL,
  name_first       VARCHAR(160) NOT NULL,
  name_middle      VARCHAR(160) DEFAULT NULL,
  name_last        VARCHAR(160) NOT NULL,
  name_suffix      VARCHAR(40) DEFAULT NULL,
  name_display     VARCHAR(320) DEFAULT NULL,
  dob              DATE DEFAULT NULL,
  status           user_profile_status NOT NULL DEFAULT 'Inactive',
  created_at       TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TRIGGER user_profile_set_updated_at
BEFORE UPDATE ON user_profile
FOR EACH ROW EXECUTE FUNCTION set_updated_at();
