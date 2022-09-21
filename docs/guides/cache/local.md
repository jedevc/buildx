# Local cache storage

The `local` cache store is a simple cache option that stores your cache as files
in a directory on your filesystem, using an
[OCI image layout](https://github.com/opencontainers/image-spec/blob/main/image-layout.md)
for the underlying directory structure. Local cache is a good choice if you're
just testing, or if you want the flexibility to self-manage a shared storage
solution.

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

```console
$ docker buildx build . --push -t <user>/<image> \
  --cache-to type=local,dest=path/to/local/dir,mode=...,compression=...,oci-mediatypes=... \
  --cache-from type=local,src=path/to/local/dir
```

The supported parameters are:

- `dest`: absolute or relative path to the local directory where you want to
  export the cache to.
- `src`: absolute or relative path to the local directory where you want to
  import cache from.

  If the `src` cache doesn't exist, then the cache import step will fail, but
  the build will continue.

- `mode`: see [cache mode](./index.md#cache-mode)
- `compression`: see [cache compression](./index.md#cache-compression)
- `oci-mediatypes`: see [OCI media types](./index.md#oci-media-types)
- `digest`: see [cache versioning](#cache-versioning)

## Cache versioning

This section describes how versioning works for caches on a local filesystem,
and how you can use the `digest` parameter to use older versions of cache.

If you inspect the cache directory manually, you can see the resulting OCI image
layout:

```console
$ ls cache
blobs  index.json  ingest
$ cat cache/index.json | jq
{
  "schemaVersion": 2,
  "manifests": [
    {
      "mediaType": "application/vnd.oci.image.index.v1+json",
      "digest": "sha256:6982c70595cb91769f61cd1e064cf5f41d5357387bab6b18c0164c5f98c1f707",
      "size": 1560,
      "annotations": {
        "org.opencontainers.image.ref.name": "latest"
      }
    }
  ]
}
```

Like other cache types, local cache gets replaced on export, by replacing the
contents of the `index.json` file. However, previous caches will still be
available in the `blobs` directory. These old caches are addressable by digest,
and kept indefinitely. Therefore, the size of the local cache will continue to
grow (see [`moby/buildkit#1896`](https://github.com/moby/buildkit/issues/1896)
for more information).

When importing cache using `--cache-to`, you can specify the `digest` parameter
to force loading an older version of the cache, for example:

```console
$ docker buildx build . --push -t <user>/<image> \
  --cache-to type=local,dest=path/to/local/dir \
  --cache-from type=local,ref=path/to/local/dir,digest=sha256:6982c70595cb91769f61cd1e064cf5f41d5357387bab6b18c0164c5f98c1f707
```

## Further reading

For an introduction to caching see
[Optimizing builds with cache management](https://docs.docker.com/build/building/cache).

For more information on the `local` cache backend, see the
[BuildKit README](https://github.com/moby/buildkit#local-directory-1).
