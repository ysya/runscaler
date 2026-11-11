FROM docker.io/library/node@sha256:ec318aabf188af4e5488847eea4b24452650b9917140563e140762526a37f1df
COPY index.js /probe/index.js
COPY post-cache.js /probe/post-cache.js
COPY --chmod=755 post.sh /usr/local/bin/post.sh
ENTRYPOINT ["node", "/probe/index.js"]
