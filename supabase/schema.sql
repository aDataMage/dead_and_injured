-- Dead & Injured: database setup. Run once in Supabase → SQL Editor → New query → Run.
-- Safe to run again.

-- One row per finished solo game. Only the game server writes here (with the secret key).
create table if not exists public.solo_games (
  id         bigint generated always as identity primary key,
  user_id    uuid not null references auth.users (id) on delete cascade,
  mode       text not null check (mode in ('classic', 'timed')),
  won        boolean not null,
  ms         integer,            -- solve time in milliseconds; null when time ran out
  guesses    integer not null,
  created_at timestamptz not null default now()
);

create index if not exists solo_games_user_mode_time
  on public.solo_games (user_id, mode, created_at desc);

-- Row level security: players may read only their own games; nobody can write from the browser.
alter table public.solo_games enable row level security;
drop policy if exists "players read own games" on public.solo_games;
create policy "players read own games" on public.solo_games
  for select to authenticated using ((select auth.uid()) = user_id);

-- Profile numbers per mode: games played, wins, fastest, and average of the last 30 wins.
create or replace function public.solo_stats(uid uuid)
returns json
language sql
stable
set search_path = ''
as $$
  select coalesce(json_object_agg(mode, stats), '{}'::json)
  from (
    select g.mode, json_build_object(
      'played', count(*),
      'won',    count(*) filter (where g.won),
      'best',   min(g.ms) filter (where g.won),
      'avg',    (select round(avg(r.ms)) from (
                   select s.ms from public.solo_games s
                   where s.user_id = uid and s.mode = g.mode and s.won
                   order by s.created_at desc limit 30) r),
      'recent', (select count(*) from (
                   select 1 from public.solo_games s
                   where s.user_id = uid and s.mode = g.mode and s.won
                   order by s.created_at desc limit 30) r)
    ) as stats
    from public.solo_games g
    where g.user_id = uid
    group by g.mode
  ) per_mode;
$$;

-- Only the game server may call it; browsers can't read other players' stats through it.
revoke execute on function public.solo_stats(uuid) from public, anon, authenticated;
grant execute on function public.solo_stats(uuid) to service_role;
