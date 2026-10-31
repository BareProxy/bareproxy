#!/usr/bin/env python3
# Copyright 2026 BareProxy.com
# SPDX-License-Identifier: Apache-2.0
#
# certs.py: test certificate authorities and certificates for fakenet.py.
#
#   certs.py ca   DIR NAME "Common Name"              -> DIR/NAME-ca.pem, NAME-ca.key
#   certs.py leaf DIR CA OUT DAYS NAME...              -> DIR/OUT.pem, OUT.key
#
# DAYS is how long from now the certificate lasts; a negative number gives
# one that has already ended. Nothing here is trusted outside the case study.

import datetime, sys
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.x509.oid import NameOID

now = datetime.datetime.now(datetime.timezone.utc)


def write(path, key, cert):
    with open(path + ".key", "wb") as f:
        f.write(key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.NoEncryption()))
    with open(path + ".pem", "wb") as f:
        f.write(cert.public_bytes(serialization.Encoding.PEM))


def load(path):
    key = serialization.load_pem_private_key(open(path + ".key", "rb").read(), None)
    return key, x509.load_pem_x509_certificate(open(path + ".pem", "rb").read())


if sys.argv[1] == "ca":
    d, name, cn = sys.argv[2:5]
    key = ec.generate_private_key(ec.SECP256R1())
    subj = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, cn), x509.NameAttribute(NameOID.ORGANIZATION_NAME, "Case Study")])
    cert = (x509.CertificateBuilder().subject_name(subj).issuer_name(subj).public_key(key.public_key())
            .serial_number(x509.random_serial_number()).not_valid_before(now - datetime.timedelta(days=1))
            .not_valid_after(now + datetime.timedelta(days=3650))
            .add_extension(x509.BasicConstraints(ca=True, path_length=None), critical=True)
            .add_extension(x509.KeyUsage(False, False, False, False, False, True, True, False, False), critical=True)
            .sign(key, hashes.SHA256()))
    write(f"{d}/{name}-ca", key, cert)
else:
    d, ca, out, days = sys.argv[2:6]
    names = sys.argv[6:]
    cakey, cacert = load(f"{d}/{ca}-ca")
    key = ec.generate_private_key(ec.SECP256R1())
    end = now + datetime.timedelta(days=float(days))
    start = min(now - datetime.timedelta(days=60), end - datetime.timedelta(days=30))
    cert = (x509.CertificateBuilder().subject_name(x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, names[0])]))
            .issuer_name(cacert.subject).public_key(key.public_key()).serial_number(x509.random_serial_number())
            .not_valid_before(start).not_valid_after(end)
            .add_extension(x509.SubjectAlternativeName([x509.DNSName(n) for n in names]), critical=False)
            .add_extension(x509.ExtendedKeyUsage([x509.oid.ExtendedKeyUsageOID.SERVER_AUTH]), critical=False)
            .sign(cakey, hashes.SHA256()))
    write(f"{d}/{out}", key, cert)
