import { createServer } from 'node:https';
import { readFileSync } from 'node:fs';

const issuer = process.env.OAUTH_ISSUER;
const jwks = readFileSync('/fixture/jwks.json');
const server = createServer({
  key: readFileSync('/fixture/issuer.key'),
  cert: readFileSync('/fixture/issuer.crt'),
}, (request, response) => {
  response.setHeader('Content-Type', 'application/json');
  if (request.url === '/.well-known/openid-configuration') {
    response.end(JSON.stringify({ issuer, jwks_uri: `${issuer}/jwks` }));
  } else if (request.url === '/jwks') {
    response.end(jwks);
  } else {
    response.writeHead(404);
    response.end('{}');
  }
});
server.listen(8443, '0.0.0.0');
