# Registry cache storage

The `registry` cache storage can be thought of as an extension to the `inline`
cache. Unlike the `inline` cache, the `registry` cache is entirely separate from
the image, which allows for more flexible usage - `registry`-backed cache can do
everything that the inline cache can do, and more:

- Allows for separating the cache and resulting image artifacts so that you can
  distribute your final image without the cache inside.
- It can efficiently cache multi-stage builds in `max` mode, instead of only the
  final stage.
- It works with other exporters for more flexibility, instead of only the
  `image` exporter.

> **Note**
>
> This cache storage backend requires using a different driver than the default
> `docker` driver - see more information on selecting a driver
> [here](../drivers/index.md). To create a new driver (which can act as a simple
> drop-in replacement):
>
> ```console
> docker buildx create --use --driver=docker-container
> ```

## Synopsis

Unlike the simpler `inline` cache, the `registry` cache supports several
configuration parameters:

```console
$ docker buildx build . --push -t <user>/<image> \
  --cache-to type=registry,ref=...,mode=...,compression=...,oci-mediatypes=... \
  --cache-from type=registry,ref=...
```

- `ref`: full name (registry URI, namespace, and name) of the cache image that
  you want to import from, or export to.
- `mode`: see [cache mode](./index.md#cache-mode)
- `compression`: see [cache compression](./index.md#cache-compression)
- `oci-mediatypes`:  see [OCI media types](./index.md#oci-media-types)

You can choose any valid value for `ref`, as long as it's not the same as the
target location that you push your image to. You might choose different tags
(e.g. `foo/bar:latest` and `foo/bar:build-cache`), separate image names (e.g.
`foo/bar` and `foo/bar-cache`), or even different repositories (e.g.
`docker.io/foo/bar` and `ghcr.io/foo/bar`). It's up to you to decide the
strategy that you want to use for separating your image from your cache images.

If the `--cache-from` target doesn't exist, then the cache import step will
fail, but the build will continue.

## Further reading

For an introduction to caching see
[Optimizing builds with cache management](https://docs.docker.com/build/building/cache).

For more information on the `registry` cache backend, see the
[BuildKit README](https://github.com/moby/buildkit#registry-push-image-and-cache-separately).
