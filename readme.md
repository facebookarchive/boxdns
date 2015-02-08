boxdns [![Build Status](https://secure.travis-ci.org/facebookgo/boxdns.svg)](https://travis-ci.org/facebookgo/boxdns)
======

boxdns provides a DNS server suitable for use in development along with docker.
It provides an alternative to linking.

The typical usecase where boxdns is useful is where one spins up a number of
containers representing an "instance". Imagine something like a mysql container,
memcache container, and an application container. The instances use a "prefix"
to allow isolation from other instances. So for example the relevant container
names may be "dev-mysql", "dev-memcached", "dev-app". Within the containers
we want to be able to reference them simply as "mysql.myapp.com" and
"memcached.myapp.com". To achive this, we can spin up a boxdns server with

```
boxdns -prefix=/dev- -domain=.myapp.com
```

Further the application container needs to be started with its DNS servers
configured to use this `boxdns` instance.

TODO: provide a boxdns container.

Documentation: https://godoc.org/github.com/facebookgo/boxdns
