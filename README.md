# Box Set Collections for Silo

A Silo plugin that shows every box set as a collection. Each box set Silo has formed in a movie library gets a matching collection in a **Box Sets** group under that library's Collections. Franchises then appear wherever collections do: the Collections tab, collection rows on Home, and Jellyfin apps.

It reads only the box set data Silo already stores (title, overview, poster, backdrop and movies), so it makes no TMDB requests of its own.

## What it does

On each run, for every movie library:

1. Looks up which box set each movie belongs to. Answers are cached, so routine runs only look up new or refreshed movies.
2. Creates a collection for each box set that forms. Silo forms a box set once the library has enough of its movies and box sets are turned on for the library.
3. Keeps each collection's title, description and movies (in collection order) the same as its box set.
4. Removes a collection once its box set no longer forms.
5. Sets the box set's poster and backdrop on a collection only while the collection has none of its own. The plugin treats Silo's automatic collage as no poster.

The plugin owns collections with the slug `box-set-<id>` and the group with the slug `box-sets`. It never touches other collections. You can still change visibility, featuring, placement and artwork as usual; only the title, description and movies are overwritten.

If you ran a Silo build with box set collections built in, those collections use the same slugs. The plugin adopts them instead of making duplicates.

## When it runs

- **On a schedule, or when you press Run now** under **Admin → Scheduled Tasks**. This is a full run that looks up every movie again. By default the task runs at server start; add a daily schedule there.
- **After library changes.** About 20 seconds after a library scan or a metadata refresh, a quick run checks new and refreshed movies. A burst of events gives one run.

## Setup

1. Turn on **Box sets** for your movie libraries in the library settings, if they are not on already.
2. In **Admin → API Keys**, create a key with **no scopes** for the server Owner account, or for another administrator. Silo has no narrower scope that covers collections. Profile PINs don't matter, because API keys skip them. The Owner account is the best choice: other admins can't revoke or change keys on it.
3. Install the plugin from **Admin → Plugins**, then paste the key under its **Settings**. You can also change the group name there. **Silo URL** is needed only if the plugin can't reach Silo on the address the server reports for itself.
4. Under **Admin → Scheduled Tasks**, run **Sync box set collections** once and give it a daily schedule.

## Building

```bash
make build-all
```

Releases are built by the **Release** workflow, which publishes linux amd64 and arm64 binaries.
