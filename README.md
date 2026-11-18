# Box Set Collections for Silo

A Silo plugin that shows every box set as a collection. Each box set Silo has formed in a movie library gets a matching collection in a **Box Sets** group under that library's Collections. Franchises then appear wherever collections do: the Collections tab, collection rows on Home, and Jellyfin apps.

It reads only the box set data Silo already stores (title, overview, poster, backdrop and movies), so it makes no TMDB requests of its own.

## What it does

On each run, for every movie library:

1. Looks up which box set each movie belongs to. One answer covers every movie in that box set, and answers are cached, so routine runs only look up new or refreshed movies.
2. Creates a collection for each box set that forms. Silo forms a box set once the library has enough of its movies and box sets are turned on for the library.
3. Keeps each collection's title, description and movies (in collection order) the same as its box set.
4. Removes a collection once its box set no longer forms, and only when Silo confirms it for that box set. Removals also need a run that looked up every movie successfully, and never happen when a run would remove more than half of a library's box set collections. Those cases are listed in the task's output, and you can delete the collections by hand if the removal was intended.
5. Gives each collection the box set's poster and backdrop, and swaps them when TMDB's artwork changes. If you upload your own image, it is left alone. An image that can't be fetched is tried again on the next run. Silo fetches the image itself when it can. Otherwise the plugin downloads and uploads it, for example when artwork is served from a storage server on your LAN.

A failure on one collection, such as a removal Silo refuses because a Home row still shows the collection, is listed in the task's output and the run moves on to the next collection.

The plugin owns collections with the slug `box-set-<id>` and the group with the slug `box-sets`. It never touches other collections. You can still change visibility, featuring, placement and artwork as usual; only the title, description and movies are overwritten. Don't change a box set collection's slug: the plugin finds its collections by slug, so a renamed one is treated as yours and a new one is created alongside it.

If you ran a Silo build with box set collections built in, those collections use the same slugs. The plugin adopts them instead of making duplicates.

## When it runs

- **On a schedule, or when you press Run now** under **Admin → Scheduled Tasks**. This is a full run that looks up every movie again. By default the task runs at server start; add a daily schedule there.
- **After library changes.** About 20 seconds after a library scan or a metadata refresh, a quick run checks new and refreshed movies. A burst of events gives one run.

## Before you start

- **The API key has full admin power.** Silo's API key scopes don't cover collections, so the key must have no scopes, and an unscoped key can do anything its account can. Use the server Owner account, keep the key for this plugin only, and revoke it in **Admin → API Keys** if you uninstall the plugin.
- **The collections show what that account's primary profile can see.** Box sets are worked out per viewer, so if that profile has a rating limit or library restrictions, the movies it can't see are left out of the collections for everyone. Use an account whose primary profile has no limits. The Owner's usually doesn't.
- **Turning box sets on, or lowering the minimum number of movies, shows up on the next full run.** Press Run now after changing either.

## Setup

1. Turn on **Box sets** for your movie libraries in the library settings, if they are not on already.
2. In **Admin → API Keys**, create a key with **no scopes** for the server Owner account, or for another administrator. Profile PINs don't matter, because API keys skip them. The Owner account is the best choice: other admins can't revoke or change keys on it.
3. Install the plugin from **Admin → Plugins**, then paste the key under its **Settings**. You can also change the group name there. **Silo URL** is needed only if the plugin can't reach Silo on the address the server reports for itself.
4. Restart Silo, from **Admin → Maintenance** or by restarting the container. Silo only lists a plugin's scheduled tasks at startup, and the restart also runs the first sync.
5. Under **Admin → Scheduled Tasks**, find **Sync box set collections (neilmc78.collections.boxsets)** in the System category and give it a daily schedule. Silo's own **Sync Movie Box Sets** is a different task: it builds the box set data this plugin reads.

The plugin stays under Silo's rate limit for API keys (10 requests a second, half a standard key's allowance), so a first run on a large library takes a few minutes. Later runs only look up new movies and box sets.

## Building

```bash
make build-all
```

Releases are built by the **Release** workflow, which publishes linux amd64 and arm64 binaries.
