"""Train a small MLP on MNIST; log metrics as JSON lines for MLDojo."""
import argparse
import json
import os

import torch
from torch import nn
from torchvision import datasets, transforms

ap = argparse.ArgumentParser()
ap.add_argument("--lr", type=float, default=0.1)
ap.add_argument("--epochs", type=int, default=3)
ap.add_argument("--batch", type=int, default=128)
ap.add_argument("--seed", type=int, default=0)
args = ap.parse_args()
torch.manual_seed(args.seed)
dev = "cuda" if torch.cuda.is_available() else "cpu"

tf = transforms.Compose([transforms.ToTensor(), transforms.Normalize((0.1307,), (0.3081,))])
train = torch.utils.data.DataLoader(datasets.MNIST("data", train=True, download=True, transform=tf),
                                    batch_size=args.batch, shuffle=True)
test = torch.utils.data.DataLoader(datasets.MNIST("data", train=False, download=True, transform=tf),
                                   batch_size=1000)
model = nn.Sequential(nn.Flatten(), nn.Linear(784, 256), nn.ReLU(), nn.Linear(256, 10)).to(dev)
opt = torch.optim.SGD(model.parameters(), lr=args.lr, momentum=0.9)
loss_fn = nn.CrossEntropyLoss()

os.makedirs("outputs", exist_ok=True)
log = open("outputs/metrics.jsonl", "a")
step = 0
for epoch in range(1, args.epochs + 1):
    model.train()
    for x, y in train:
        x, y = x.to(dev), y.to(dev)
        opt.zero_grad()
        loss = loss_fn(model(x), y)
        loss.backward()
        opt.step()
        step += 1
        if step % 50 == 0:
            log.write(json.dumps({"step": step, "train/loss": loss.item()}) + "\n")
            log.flush()
    model.eval()
    correct = 0
    with torch.no_grad():
        for x, y in test:
            correct += (model(x.to(dev)).argmax(1) == y.to(dev)).sum().item()
    acc = correct / len(test.dataset)
    log.write(json.dumps({"step": step, "epoch": epoch, "eval/acc": acc}) + "\n")
    log.flush()
    print(f"epoch {epoch}/{args.epochs} step {step} loss={loss.item():.4f} acc={acc:.4f}", flush=True)
    torch.save(model.state_dict(), f"outputs/model_epoch{epoch}.pt")
