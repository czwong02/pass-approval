create table if not exists tenants (
  id text primary key,
  name text not null,
  api_key_hash text not null unique,
  callback_secret text not null
);

create table if not exists users (
  id text not null,
  tenant_id text not null references tenants (id) on delete cascade,
  name text not null,
  primary key (tenant_id, id)
);

create table if not exists channel_identities (
  tenant_id text not null,
  user_id text not null,
  channel text not null,
  external_user_id text not null,
  linked_at timestamptz not null default now(),
  primary key (tenant_id, user_id, channel),
  unique (channel, external_user_id),
  foreign key (tenant_id, user_id) references users (tenant_id, id) on delete cascade
);

create table if not exists requests (
  id text primary key,
  tenant_id text not null references tenants (id) on delete cascade,
  external_ref text not null,
  title text not null,
  body text not null default '',
  fields jsonb not null default '[]'::jsonb,
  rule text not null,
  steps jsonb not null,
  status text not null,
  callback_url text not null,
  expires_at timestamptz not null,
  decided_at timestamptz,
  unique (tenant_id, external_ref)
);

create index if not exists requests_pending_expiry on requests (expires_at) where status = 'pending';

create table if not exists messages (
  request_id text not null references requests (id) on delete cascade,
  channel text not null,
  external_user_id text not null,
  message_id text not null,
  primary key (request_id, channel, external_user_id)
);

create table if not exists decisions (
  request_id text not null references requests (id) on delete cascade,
  step_index integer not null,
  approver_id text not null,
  action text not null,
  channel text not null,
  comment text not null default '',
  created_at timestamptz not null default now(),
  unique (request_id, step_index, approver_id)
);

create table if not exists action_tokens (
  token_hash text primary key,
  request_id text not null references requests (id) on delete cascade,
  approver_id text not null,
  action text not null,
  step_index integer not null,
  used_at timestamptz
);

create table if not exists callback_outbox (
  id bigint generated always as identity primary key,
  request_id text not null references requests (id) on delete cascade,
  body jsonb not null,
  attempts integer not null default 0,
  next_at timestamptz not null default now(),
  dead boolean not null default false
);

create table if not exists link_codes (
  code_hash text primary key,
  tenant_id text not null,
  user_id text not null,
  expires_at timestamptz not null,
  used_at timestamptz,
  foreign key (tenant_id, user_id) references users (tenant_id, id) on delete cascade
);

insert into tenants (id, name, api_key_hash, callback_secret)
values (
  'ten_dev',
  'dev',
  '7e9f8fd111802be56c379d597842e29b2cebd35ff2133d431a49fa556a18704e',
  'dev-callback-secret'
)
on conflict (id) do nothing;

insert into users (id, tenant_id, name) values
  ('ada', 'ten_dev', 'Ada'),
  ('bob', 'ten_dev', 'Bob'),
  ('chao', 'ten_dev', 'Chao')
on conflict (tenant_id, id) do nothing;
